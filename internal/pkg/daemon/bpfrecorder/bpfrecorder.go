//go:build linux && !no_bpf
// +build linux,!no_bpf

/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bpfrecorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	bpf "github.com/aquasecurity/libbpfgo"
	"github.com/blang/semver/v4"
	"github.com/go-logr/logr"
	"github.com/jellydator/ttlcache/v3"
	seccomp "github.com/seccomp/libseccomp-golang"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"

	api "sigs.k8s.io/security-profiles-operator/api/grpc/bpfrecorder"
	apimetrics "sigs.k8s.io/security-profiles-operator/api/grpc/metrics"
	"sigs.k8s.io/security-profiles-operator/internal/pkg/bimap"
	"sigs.k8s.io/security-profiles-operator/internal/pkg/config"
	"sigs.k8s.io/security-profiles-operator/internal/pkg/daemon/bpfrecorder/types"
	"sigs.k8s.io/security-profiles-operator/internal/pkg/util"
)

const (
	defaultTimeout      time.Duration = time.Minute
	maxMsgSize          int           = 16 * 1024 * 1024
	defaultCacheTimeout time.Duration = time.Hour
	maxCacheItems       uint64        = 1000
	defaultHostPid      uint32        = 1
	defaultByteNum      int           = 4
)

// BpfRecorder is the main structure of this package.
type BpfRecorder struct {
	api.UnimplementedBpfRecorderServer
	impl
	logger                  logr.Logger
	startRequests           int64
	syscalls                *bpf.BPFMap
	mntns                   *bpf.BPFMap
	btfPath                 string
	syscallIDtoNameCache    *ttlcache.Cache[string, string]
	pidToContainerIDCache   *ttlcache.Cache[string, string]
	mntnsToContainerIDMap   *bimap.BiMap[uint32, string]
	containerIDToProfileMap *bimap.BiMap[string, string]
	nodeName                string
	clientset               *kubernetes.Clientset
	systemMountNamespace    uint32
	loadUnloadMutex         sync.RWMutex
	metricsClient           apimetrics.Metrics_BpfIncClient
	programNameFilter       string
}

// New returns a new BpfRecorder instance.
func New(logger logr.Logger) *BpfRecorder {
	return &BpfRecorder{
		impl:   &defaultImpl{},
		logger: logger,
		syscallIDtoNameCache: ttlcache.New(
			ttlcache.WithTTL[string, string](defaultCacheTimeout),
			ttlcache.WithCapacity[string, string](maxCacheItems),
		),
		pidToContainerIDCache: ttlcache.New(
			ttlcache.WithTTL[string, string](defaultCacheTimeout),
			ttlcache.WithCapacity[string, string](maxCacheItems),
		),
		mntnsToContainerIDMap:   bimap.New[uint32, string](),
		containerIDToProfileMap: bimap.New[string, string](),
		loadUnloadMutex:         sync.RWMutex{},
	}
}

// Syscalls returns the bpf map containing the PID (key) to syscalls (value)
// data.
func (b *BpfRecorder) Syscalls() *bpf.BPFMap {
	return b.syscalls
}

// FilterProgramName can be used to filter on a specific program name.
func (b *BpfRecorder) FilterProgramName(filter string) {
	b.programNameFilter = filepath.Base(filter)
}

// Run the BpfRecorder.
func (b *BpfRecorder) Run() error {
	b.logger.Info(fmt.Sprintf("Setting up caches with expiry of %v", defaultCacheTimeout))
	for _, cache := range []*ttlcache.Cache[string, string]{
		b.pidToContainerIDCache,
		b.syscallIDtoNameCache,
	} {
		go cache.Start()
	}

	b.nodeName = b.Getenv(config.NodeNameEnvKey)
	if b.nodeName == "" {
		err := fmt.Errorf("%s environment variable not set", config.NodeNameEnvKey)
		b.logger.Error(err, "unable to run recorder")
		return err
	}
	b.logger.Info("Starting log-enricher on node: " + b.nodeName)

	clusterConfig, err := b.InClusterConfig()
	if err != nil {
		return fmt.Errorf("get in-cluster config: %w", err)
	}

	b.clientset, err = b.NewForConfig(clusterConfig)
	if err != nil {
		return fmt.Errorf("load in-cluster client: %w", err)
	}

	if _, err := b.Stat(config.GRPCServerSocketBpfRecorder); err == nil {
		if err := b.RemoveAll(config.GRPCServerSocketBpfRecorder); err != nil {
			return fmt.Errorf("remove GRPC socket file: %w", err)
		}
	}

	listener, err := b.Listen("unix", config.GRPCServerSocketBpfRecorder)
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}

	if err := b.Chown(
		config.GRPCServerSocketBpfRecorder,
		config.UserRootless,
		config.UserRootless,
	); err != nil {
		return fmt.Errorf("change GRPC socket owner to rootless: %w", err)
	}

	b.logger.Info("Connecting to metrics server")
	conn, cancel, err := b.connectMetrics()
	if err != nil {
		return fmt.Errorf("connect to metrics server: %w", err)
	}
	if cancel != nil {
		defer cancel()
	}
	if conn != nil {
		defer func() {
			if err := b.CloseGRPC(conn); err != nil {
				b.logger.Error(err, "unable to close GRPC connection")
			}
		}()
	}

	b.systemMountNamespace, err = b.FindProcMountNamespace(defaultHostPid)
	if err != nil {
		return fmt.Errorf("retrieve current mount namespace: %w", err)
	}
	b.logger.Info("Got system mount namespace: " + fmt.Sprint(b.systemMountNamespace))

	b.logger.Info("Doing BPF load/unload self-test")
	if err := b.Load(false); err != nil {
		return fmt.Errorf("load self-test: %w", err)
	}
	b.Unload()

	b.logger.Info("Starting GRPC API server")
	grpcServer := grpc.NewServer(
		grpc.MaxSendMsgSize(maxMsgSize),
		grpc.MaxRecvMsgSize(maxMsgSize),
	)
	api.RegisterBpfRecorderServer(grpcServer, b)

	return b.Serve(grpcServer, listener)
}

func (b *BpfRecorder) connectMetrics() (conn *grpc.ClientConn, cancel context.CancelFunc, err error) {
	if err := util.Retry(func() (err error) {
		conn, cancel, err = b.DialMetrics()
		if err != nil {
			return fmt.Errorf("connecting to local metrics GRPC server: %w", err)
		}
		client := apimetrics.NewMetricsClient(conn)

		b.metricsClient, err = b.BpfIncClient(client)
		if err != nil {
			cancel()
			if err := b.CloseGRPC(conn); err != nil {
				b.logger.Error(err, "Unable to close GRPC connection")
			}
			return fmt.Errorf("create metrics bpf client: %w", err)
		}

		return nil
	}, func(err error) bool { return true }); err != nil {
		return nil, nil, fmt.Errorf("connect to local GRPC server: %w", err)
	}

	return conn, cancel, nil
}

// Dial can be used to connect to the default GRPC server by creating a new
// client.
func Dial() (*grpc.ClientConn, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	conn, err := grpc.DialContext(
		ctx,
		"unix://"+config.GRPCServerSocketBpfRecorder,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("GRPC dial: %w", err)
	}
	return conn, cancel, nil
}

func (b *BpfRecorder) Start(
	ctx context.Context, r *api.EmptyRequest,
) (*api.EmptyResponse, error) {
	if b.startRequests == 0 {
		b.logger.Info("Starting bpf recorder")
		//nolint:contextcheck // no context intended here
		if err := b.Load(true); err != nil {
			return nil, fmt.Errorf("load bpf: %w", err)
		}
	} else {
		b.logger.Info("bpf recorder already running")
	}

	atomic.AddInt64(&b.startRequests, 1)
	return &api.EmptyResponse{}, nil
}

func (b *BpfRecorder) Stop(
	ctx context.Context, r *api.EmptyRequest,
) (*api.EmptyResponse, error) {
	if b.startRequests == 0 {
		b.logger.Info("bpf recorder not running")
		return &api.EmptyResponse{}, nil
	}

	atomic.AddInt64(&b.startRequests, -1)
	if b.startRequests == 0 {
		b.logger.Info("Stopping bpf recorder")
		b.Unload()
	} else {
		b.logger.Info("Not stopping because another recording is in progress")
	}
	return &api.EmptyResponse{}, nil
}

// SyscallsForProfile returns the syscall names for the provided profile name.
func (b *BpfRecorder) SyscallsForProfile(
	ctx context.Context, r *api.ProfileRequest,
) (*api.SyscallsResponse, error) {
	if b.startRequests == 0 {
		return nil, errors.New("bpf recorder not running")
	}
	b.logger.Info("Getting syscalls for profile " + r.Name)

	// There is a chance to miss the PID if concurrent processes are being
	// analyzed. If we request the `SyscallsForProfile` exactly between two
	// events, while the first one is from a different recording container and
	// we have to expect the profile in the second event. We try to overcome
	// this race by retrying, but with a more loose backoff strategy than
	// retrying to retrieve the in-cluster container ID.
	var (
		mntns uint32
		try   = -1
	)
	if err := util.Retry(
		func() error {
			try++
			b.logger.Info("Looking up mount namespace for profile", "try", try, "profile", r.Name)

			if foundMntns, ok := b.getMntnsForProfile(r.Name); ok {
				mntns = foundMntns
				b.logger.Info("Found mount namespace for profile", "mntns", mntns, "profile", r.Name)
				b.deleteContainerIDFromCache(r.Name)
				return nil
			}

			b.logger.Info("No mount namespace found for profile", "profile", r.Name)
			return ErrNotFound
		},
		func(error) bool { return true },
	); err != nil {
		return nil, ErrNotFound
	}

	b.loadUnloadMutex.RLock()
	syscalls, err := b.GetValue(b.syscalls, mntns)
	b.loadUnloadMutex.RUnlock()
	if err != nil {
		b.logger.Error(err, "No syscalls found for mntns", "mntns", mntns)
		return nil, fmt.Errorf("no syscalls found for mntns: %d", mntns)
	}
	syscallNames := b.convertSyscallIDsToNames(syscalls)

	// Cleanup the syscalls map from eBpf.
	b.logger.Info("Cleaning up BPF syscalls hashmaps")
	b.loadUnloadMutex.Lock()
	if err := b.DeleteKey(b.syscalls, mntns); err != nil {
		b.logger.Error(err, "Unable to cleanup syscalls map", "mntns", mntns)
	}
	b.loadUnloadMutex.Unlock()

	return &api.SyscallsResponse{
		Syscalls: sortUnique(syscallNames),
		GoArch:   runtime.GOARCH,
	}, nil
}

func (b *BpfRecorder) getMntnsForProfile(profile string) (uint32, bool) {
	if containerID, ok := b.containerIDToProfileMap.GetBackwards(profile); ok {
		if mntns, ok := b.mntnsToContainerIDMap.GetBackwards(containerID); ok {
			return mntns, true
		}
	}
	return 0, false
}

func (b *BpfRecorder) deleteContainerIDFromCache(profile string) {
	if containerID, ok := b.containerIDToProfileMap.GetBackwards(profile); ok {
		b.containerIDToProfileMap.Delete(containerID)
		b.mntnsToContainerIDMap.DeleteBackwards(containerID)
	}
}

func (b *BpfRecorder) convertSyscallIDsToNames(syscalls []byte) []string {
	result := []string{}
	for id, set := range syscalls {
		if set == 1 {
			name, err := b.syscallNameForID(id)
			if err != nil {
				b.logger.Error(err, "unable to convert syscall ID")
				continue
			}
			result = append(result, name)
		}
	}
	return result
}

func sortUnique(input []string) (result []string) {
	tmp := map[string]bool{}
	for _, val := range input {
		tmp[val] = true
	}
	for k := range tmp {
		result = append(result, k)
	}
	sort.Strings(result)
	return result
}

// Load prestarts the bpf recorder.
func (b *BpfRecorder) Load(startEventProcessor bool) (err error) {
	b.logger.Info("Loading bpf module")
	b.btfPath, err = b.findBtfPath()
	if err != nil {
		return fmt.Errorf("find btf: %w", err)
	}

	bpfObject, ok := bpfObjects[b.GoArch()]
	if !ok {
		return fmt.Errorf("architecture %s is currently unsupported", runtime.GOARCH)
	}

	module, err := b.NewModuleFromBufferArgs(&bpf.NewModuleArgs{
		BPFObjBuff: bpfObject,
		BPFObjName: "recorder.bpf.o",
		BTFObjPath: b.btfPath,
	})
	if err != nil {
		return fmt.Errorf("load bpf module: %w", err)
	}

	if b.programNameFilter != "" {
		if err := b.InitGlobalVariable(
			module, "filter_name", []byte(b.programNameFilter),
		); err != nil {
			return fmt.Errorf("init global variable: %w", err)
		}
	}

	b.logger.Info("Loading bpf object from module")
	if err := b.BPFLoadObject(module); err != nil {
		return fmt.Errorf("load bpf object: %w", err)
	}

	const programName = "sys_enter"
	b.logger.Info("Getting bpf program " + programName)
	program, err := b.GetProgram(module, programName)
	if err != nil {
		return fmt.Errorf("get %s program: %w", programName, err)
	}

	b.logger.Info("Attaching bpf tracepoint")
	if _, err := b.AttachTracepoint(program, "raw_syscalls", programName); err != nil {
		return fmt.Errorf("attach tracepoint: %w", err)
	}

	b.logger.Info("Getting syscalls map")
	syscalls, err := b.GetMap(module, "mntns_syscalls")
	if err != nil {
		return fmt.Errorf("get syscalls map: %w", err)
	}

	b.logger.Info("Getting pid_mntns map")
	mntns, err := b.GetMap(module, "pid_mntns")
	if err != nil {
		return fmt.Errorf("get pid_mntns: %w", err)
	}

	b.syscalls = syscalls
	b.mntns = mntns

	// Update the host mntns into pid_mntns map
	b.updateSystemMntns()

	events := make(chan []byte)
	ringbuffer, err := b.InitRingBuf(module, "events", events)
	if err != nil {
		return fmt.Errorf("init events ringbuffer: %w", err)
	}
	b.StartRingBuffer(ringbuffer)

	if startEventProcessor {
		go b.processEvents(events)
	}

	b.logger.Info("Module successfully loaded")
	return nil
}

func (b *BpfRecorder) findBtfPath() (string, error) {
	// Use the system btf if possible
	if _, err := b.Stat("/sys/kernel/btf/vmlinux"); err == nil {
		b.logger.Info("Using system btf file")
		return "", nil
	}

	b.logger.Info("Trying to find matching in-memory btf")

	btf := types.Btf{}
	if err := b.Unmarshal([]byte(btfJSON), &btf); err != nil {
		return "", fmt.Errorf("unmarshal btf JSON: %w", err)
	}

	res, err := b.ReadOSRelease()
	if err != nil {
		return "", fmt.Errorf("read os-release file: %w", err)
	}

	osID := types.Os(res["ID"])
	btfOs, ok := btf[osID]
	if !ok {
		b.logger.Info(fmt.Sprintf("OS not found in btf map: %s", osID))
		return "", nil
	}
	b.logger.Info(fmt.Sprintf("OS found in btf map: %s", osID))

	osVersion := types.OsVersion(res["VERSION_ID"])
	btfOsVersion, ok := btfOs[osVersion]
	if !ok {
		b.logger.Info(fmt.Sprintf("OS version not found in btf map: %s", osVersion))
		return "", nil
	}
	b.logger.Info(fmt.Sprintf("OS version found in btf map: %s", osVersion))

	uname := syscall.Utsname{}
	if err := b.Uname(&uname); err != nil {
		return "", fmt.Errorf("uname syscall failed: %w", err)
	}

	arch := types.Arch(toStringInt8(uname.Machine))
	btfArch, ok := btfOsVersion[arch]
	if !ok {
		b.logger.Info(fmt.Sprintf("Architecture not found in btf map: %s", arch))
		return "", nil
	}
	b.logger.Info(fmt.Sprintf("Architecture found in btf map: %s", arch))

	release := toStringInt8(uname.Release)
	version, err := semver.Parse(release)
	if err != nil {
		return "", fmt.Errorf("unable to parse semver for release %s: %w", release, err)
	}
	version.Pre = nil
	const (
		lowestMajor = 5
		lowestMinor = 8
	)
	if version.LT(semver.Version{Major: lowestMajor, Minor: lowestMinor}) {
		return "", fmt.Errorf("unsupported kernel version %s: at least Linux 5.8 is required", release)
	}

	kernel := types.Kernel(release)
	btfBytes, ok := btfArch[kernel]
	if !ok {
		b.logger.Info(fmt.Sprintf("Kernel not found in btf map: %s", kernel))
		return "", nil
	}
	b.logger.Info(fmt.Sprintf("Kernel found in btf map: %s", kernel))

	file, err := b.TempFile(
		"",
		fmt.Sprintf("spo-btf-%s-%s-%s-%s", osID, osVersion, arch, kernel),
	)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer file.Close()

	if _, err := b.Write(file, btfBytes); err != nil {
		return "", fmt.Errorf("write BTF: %w", err)
	}

	b.logger.Info(fmt.Sprintf("Wrote BTF to file: %s", file.Name()))
	return file.Name(), nil
}

func toStringInt8(array [65]int8) string {
	var buf [65]byte
	for i, b := range array {
		buf[i] = byte(b)
	}
	return toStringByte(buf[:])
}

func toStringByte(array []byte) string {
	str := string(array)
	if i := strings.Index(str, "\x00"); i != -1 {
		str = str[:i]
	}
	return str
}

func (b *BpfRecorder) processEvents(events chan []byte) {
	b.logger.Info("Processing events")

	// Allow up to 1000 goroutines to run in parallel
	const workers = 1000
	sem := semaphore.NewWeighted(workers)

	for event := range events {
		if err := sem.Acquire(context.Background(), 1); err != nil {
			b.logger.Error(err, "Unable to acquire semaphore, stopping event processor")
			break
		}
		eventCopy := event
		go func() {
			b.handleEvent(eventCopy)
			sem.Release(1)
		}()
	}
}

func (b *BpfRecorder) updateSystemMntns() {
	mntnsByte := make([]byte, defaultByteNum)
	binary.LittleEndian.PutUint32(mntnsByte, b.systemMountNamespace)
	err := b.UpdateValue(b.mntns, defaultHostPid, mntnsByte)
	if err != nil {
		b.logger.Error(err, "update system_mntns map failed")
	}
}

func (b *BpfRecorder) handleEvent(event []byte) {
	e := struct {
		Pid   uint32
		Mntns uint32
	}{}

	if err := binary.Read(bytes.NewReader(event), binary.LittleEndian, &e); err != nil {
		b.logger.Error(err, "Unable to read event")
		return
	}

	b.logger.V(config.VerboseLevel).Info(fmt.Sprintf("Received event: pid: %d, mntns: %d", e.Pid, e.Mntns))

	pid := e.Pid
	mntns := e.Mntns

	// Look up the container ID based on PID from cgroup file.
	containerID, err := b.ContainerIDForPID(b.pidToContainerIDCache, int(pid))
	if err != nil {
		b.logger.V(config.VerboseLevel).Info(
			"No container ID found for PID",
			"pid", pid, "mntns", mntns, "err", err.Error(),
		)
		return
	}
	b.mntnsToContainerIDMap.Insert(mntns, containerID)

	b.logger.V(config.VerboseLevel).Info(
		"Found container ID for PID", "pid", pid,
		"mntns", mntns, "containerID", containerID,
	)
	profile, err := b.findProfileForContainerID(containerID)
	if err != nil {
		b.logger.Error(err, "Unable to find profile in cluster for container ID",
			"id", containerID, "pid", pid, "mntns", mntns)
		return
	}

	b.logger.Info(
		"Found profile in cluster for container ID", "containerID", containerID,
		"pid", pid, "mntns", mntns, "profile", profile,
	)

	b.trackProfileMetric(mntns, profile)
}

// FindProcMountNamespace is looking up the mnt ns for a given PID.
func (b *BpfRecorder) FindProcMountNamespace(pid uint32) (uint32, error) {
	// This requires the container to run with host PID, otherwise we will get
	// the namespace from the container.
	procLink := fmt.Sprintf("/proc/%d/ns/mnt", pid)
	res, err := b.Readlink(procLink)
	if err != nil {
		return 0, fmt.Errorf("read mount namespace link: %w", err)
	}
	stripped := strings.TrimPrefix(res, "mnt:[")
	stripped = strings.TrimSuffix(stripped, "]")

	ns, err := b.ParseUint(stripped)
	if err != nil {
		return 0, fmt.Errorf("convert namespace to integer: %w", err)
	}

	return ns, nil
}

func (b *BpfRecorder) trackProfileMetric(mntns uint32, profile string) {
	if err := b.SendMetric(b.metricsClient, &apimetrics.BpfRequest{
		Node:           b.nodeName,
		Profile:        profile,
		MountNamespace: mntns,
	}); err != nil {
		b.logger.Error(err, "Unable to update metrics")
	}
}

func (b *BpfRecorder) findProfileForContainerID(id string) (string, error) {
	if profile, ok := b.containerIDToProfileMap.Get(id); ok {
		b.logger.Info("Found profile in cache", "containerID", id, "profile", profile)
		return profile, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTimeout)
	defer cancel()

	const (
		backoffDuration = 100 * time.Millisecond
		backoffFactor   = 1.2
		backoffSteps    = 20
	)

	try := -1
	if err := util.RetryEx(
		&wait.Backoff{
			Duration: backoffDuration,
			Factor:   backoffFactor,
			Steps:    backoffSteps,
		},
		func() error {
			try++
			b.logger.Info("Looking up container ID in cluster", "id", id, "try", try)
			pods, err := b.ListPods(ctx, b.clientset, b.nodeName)
			if err != nil {
				return fmt.Errorf("list node pods: %w", err)
			}
			if pods == nil {
				return fmt.Errorf("no pods found in cluster")
			}

			for p := range pods.Items {
				pod := &pods.Items[p]
				//nolint:gocritic // We explicitly do not want to append to the same slice
				statuses := append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...)
				for c := range statuses {
					containerStatus := statuses[c]
					fullContainerID := containerStatus.ContainerID
					containerName := containerStatus.Name

					// The container ID is not yet available in the container status of the pod.
					// This container can be skipped for now, the status will be checked again later.
					if fullContainerID == "" {
						b.logger.Info(
							"Container ID not yet available in cluster",
							"containerID", id,
							"podName", pod.Name,
							"containerName", containerName,
						)
						continue
					}

					containerID := util.ContainerIDRegex.FindString(fullContainerID)
					if containerID == "" {
						b.logger.Error(err,
							"Unable to parse container ID from container status available in pod",
							"fullContainerID", fullContainerID,
							"podName", pod.Name,
							"containerName", containerName,
						)
						continue
					}

					b.logger.V(config.VerboseLevel).Info(
						"Found Container ID in cluser",
						"containerID", containerID,
						"podName", pod.Name,
						"containerName", containerName,
					)

					key := config.SeccompProfileRecordBpfAnnotationKey + containerName
					profile, ok := pod.Annotations[key]
					if ok && profile != "" {
						b.logger.Info(
							"Cache this profile found in cluster",
							"profile", profile,
							"containerID", containerID,
							"podName", pod.Name,
							"containerName", containerName,
						)
						b.containerIDToProfileMap.Insert(containerID, profile)
					}

					// Stop looking for this container ID regadless of a profile was found or not.
					if containerID == id {
						return nil
					}
				}
			}
			return fmt.Errorf("container ID not found in cluster: %s", id)
		},
		func(error) bool { return true },
	); err != nil {
		return "", fmt.Errorf("searching container ID %s: %w", id, err)
	}

	if profile, ok := b.containerIDToProfileMap.Get(id); ok {
		b.logger.Info(
			"Found profile in cluster for container ID",
			"profile", profile,
			"containerID", id,
		)
		return profile, nil
	}

	return "", fmt.Errorf("container ID not found: %s", id)
}

// Unload can be used to reset the bpf recorder.
func (b *BpfRecorder) Unload() {
	b.logger.Info("Unloading bpf module")
	b.loadUnloadMutex.Lock()
	b.CloseModule(b.syscalls)
	b.syscalls = nil
	os.RemoveAll(b.btfPath)
	b.loadUnloadMutex.Unlock()
}

func (b *BpfRecorder) syscallNameForID(id int) (string, error) {
	key := strconv.Itoa(id)
	item := b.syscallIDtoNameCache.Get(key)
	if item != nil {
		return item.Value(), nil
	}

	name, err := b.GetName(seccomp.ScmpSyscall(id))
	if err != nil {
		return "", fmt.Errorf("get syscall name for ID %d: %w", id, err)
	}

	b.syscallIDtoNameCache.Set(key, name, ttlcache.DefaultTTL)
	return name, nil
}
