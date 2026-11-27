package mcpkit

import (
	"context"
	"net/http"
	"sync"

	"connectrpc.com/connect"
	connectv2 "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// AuthCheckProcedure is the no-op rpc /mcp sends a caller's credential
// through, so the listener's chain — the project's Authenticator — judges it.
// The listener mounts it with its chain (MountAuthCheck); it is never in a
// project's unauthenticated list, so a call without a good credential is
// refused Unauthenticated by the chain's Auth slot. It has no side effects
// and answers google.protobuf.Empty.
const AuthCheckProcedure = "/forge.mcp.v1.MCPAuth/CheckCaller"

// AuthCheckSchema is AuthCheckProcedure's method descriptor (declared
// NO_SIDE_EFFECTS). It is built in memory and not registered in
// protoregistry.GlobalFiles, so it never collides with a project's protos.
func AuthCheckSchema() protoreflect.MethodDescriptor { return authCheckSchema() }

var authCheckSchema = sync.OnceValue(func() protoreflect.MethodDescriptor {
	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("forge/mcp/v1/mcp_auth.proto"),
		Package:    proto.String("forge.mcp.v1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/empty.proto"},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("MCPAuth"),
			Method: []*descriptorpb.MethodDescriptorProto{{
				Name:       proto.String("CheckCaller"),
				InputType:  proto.String(".google.protobuf.Empty"),
				OutputType: proto.String(".google.protobuf.Empty"),
				Options: &descriptorpb.MethodOptions{
					IdempotencyLevel: descriptorpb.MethodOptions_NO_SIDE_EFFECTS.Enum(),
				},
			}},
		}},
	}
	_ = emptypb.File_google_protobuf_empty_proto // the import is resolved from the registry
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		panic("mcpkit: build the auth-check descriptor: " + err.Error())
	}
	return fd.Services().Get(0).Methods().Get(0)
})

func checkCaller(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
	return connect.NewResponse(&emptypb.Empty{}), nil
}

// MountAuthCheck mounts AuthCheckProcedure on mux with opts — the listener's
// chain. A pattern already registered is kept.
func MountAuthCheck(mux *http.ServeMux, opts ...connect.HandlerOption) (mounted bool) {
	defer func() {
		if recover() != nil {
			mounted = false
		}
	}()
	opts = append([]connect.HandlerOption{connect.WithSchema(AuthCheckSchema()), connect.WithIdempotency(connect.IdempotencyNoSideEffects)}, opts...)
	mux.Handle(AuthCheckProcedure, connect.NewUnaryHandler(AuthCheckProcedure, checkCaller, opts...))
	return true
}

// AuthCheckMethodV2 is AuthCheckProcedure as a connect-go v2 method, for a
// listener whose chain is v2.
func AuthCheckMethodV2() connectv2.Method {
	return connectv2.Method{
		Spec: connectv2.Spec{
			Procedure:        AuthCheckProcedure,
			StreamType:       connectv2.StreamTypeUnary,
			Schema:           AuthCheckSchema(),
			IdempotencyLevel: connectv2.IdempotencyNoSideEffects,
		},
		Handler: func(_ context.Context, _ connectv2.Spec, stream connectv2.ServerStream) error {
			var req emptypb.Empty
			if err := stream.Receive(&req); err != nil {
				return err
			}
			return stream.Send(&emptypb.Empty{})
		},
	}
}
