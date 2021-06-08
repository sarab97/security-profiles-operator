// Code generated by protoc-gen-go-grpc. DO NOT EDIT.

package api

import (
	grpc "google.golang.org/grpc"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// SecurityProfilesOperatorClient is the client API for SecurityProfilesOperator service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type SecurityProfilesOperatorClient interface {
}

type securityProfilesOperatorClient struct {
	cc grpc.ClientConnInterface
}

func NewSecurityProfilesOperatorClient(cc grpc.ClientConnInterface) SecurityProfilesOperatorClient {
	return &securityProfilesOperatorClient{cc}
}

// SecurityProfilesOperatorServer is the server API for SecurityProfilesOperator service.
// All implementations must embed UnimplementedSecurityProfilesOperatorServer
// for forward compatibility
type SecurityProfilesOperatorServer interface {
	mustEmbedUnimplementedSecurityProfilesOperatorServer()
}

// UnimplementedSecurityProfilesOperatorServer must be embedded to have forward compatible implementations.
type UnimplementedSecurityProfilesOperatorServer struct {
}

func (UnimplementedSecurityProfilesOperatorServer) mustEmbedUnimplementedSecurityProfilesOperatorServer() {
}

// UnsafeSecurityProfilesOperatorServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to SecurityProfilesOperatorServer will
// result in compilation errors.
type UnsafeSecurityProfilesOperatorServer interface {
	mustEmbedUnimplementedSecurityProfilesOperatorServer()
}

func RegisterSecurityProfilesOperatorServer(s grpc.ServiceRegistrar, srv SecurityProfilesOperatorServer) {
	s.RegisterService(&SecurityProfilesOperator_ServiceDesc, srv)
}

// SecurityProfilesOperator_ServiceDesc is the grpc.ServiceDesc for SecurityProfilesOperator service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var SecurityProfilesOperator_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "api.SecurityProfilesOperator",
	HandlerType: (*SecurityProfilesOperatorServer)(nil),
	Methods:     []grpc.MethodDesc{},
	Streams:     []grpc.StreamDesc{},
	Metadata:    "api/server/api.proto",
}