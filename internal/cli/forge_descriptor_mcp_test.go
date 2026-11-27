package cli

import (
	"testing"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"

	"github.com/reliant-labs/forge/internal/codegen"
	forgev1 "github.com/reliant-labs/forge/pkg/forgepb"
)

// (forge.v1.method).mcp / mcp_mutating and (forge.v1.service).mcp reach
// codegen.Method.MCP through the real protogen round trip, folded: the
// rpc's value wins, the service's is the default, and nothing declared is
// nil. The leading comment becomes the Description, marker lines dropped.
func TestExtractService_RecordsMCP(t *testing.T) {
	methodOpts := func(level descriptorpb.MethodOptions_IdempotencyLevel, mo *forgev1.MethodOptions) *descriptorpb.MethodOptions {
		o := &descriptorpb.MethodOptions{IdempotencyLevel: level.Enum()}
		if mo != nil {
			proto.SetExtension(o, forgev1.E_Method, mo)
		}
		return o
	}
	method := func(name string, opts *descriptorpb.MethodOptions) *descriptorpb.MethodDescriptorProto {
		return &descriptorpb.MethodDescriptorProto{
			Name: proto.String(name), InputType: proto.String(".shop.v1.Empty"), OutputType: proto.String(".shop.v1.Empty"), Options: opts,
		}
	}
	svcOpts := &descriptorpb.ServiceOptions{}
	proto.SetExtension(svcOpts, forgev1.E_Service, &forgev1.ServiceOptions{Mcp: proto.Bool(true)})
	unknown := descriptorpb.MethodOptions_IDEMPOTENCY_UNKNOWN
	file := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("shop/v1/shop.proto"),
		Package:     proto.String("shop.v1"),
		Syntax:      proto.String("proto3"),
		Options:     &descriptorpb.FileOptions{GoPackage: proto.String("example.com/gen/shop/v1;shopv1")},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name:    proto.String("OrdersService"),
				Options: svcOpts,
				Method: []*descriptorpb.MethodDescriptorProto{
					method("ListOrders", methodOpts(descriptorpb.MethodOptions_NO_SIDE_EFFECTS, nil)),
					method("CancelOrder", methodOpts(unknown, &forgev1.MethodOptions{McpMutating: true, Idempotent: true})),
					method("GetSecret", methodOpts(unknown, &forgev1.MethodOptions{Mcp: proto.Bool(false)})),
					method("SetFlag", methodOpts(descriptorpb.MethodOptions_IDEMPOTENT, nil)),
				},
			},
			{
				Name: proto.String("StatusService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					method("GetStatus", methodOpts(unknown, &forgev1.MethodOptions{Mcp: proto.Bool(true)})),
					method("GetVersion", nil),
				},
			},
		},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{Location: []*descriptorpb.SourceCodeInfo_Location{{
			Path:            []int32{6, 0, 2, 0},
			Span:            []int32{1, 0, 1},
			LeadingComments: proto.String(" ListOrders returns the caller's orders.\n forge:custom\n Newest first.\n"),
		}}},
	}
	p, err := protogen.Options{}.New(&pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{file.GetName()},
		ProtoFile:      []*descriptorpb.FileDescriptorProto{file},
	})
	if err != nil {
		t.Fatalf("protogen.New: %v", err)
	}
	f := p.FilesByPath[file.GetName()]

	want := map[string]*codegen.MethodMCP{
		"ListOrders":  {OptIn: true},
		"CancelOrder": {OptIn: true, MutatingAck: true, Mutating: true, Idempotent: true},
		"GetSecret":   {OptIn: false},
		"SetFlag":     {OptIn: true, Mutating: true, Idempotent: true},
		"GetStatus":   {OptIn: true},
		"GetVersion":  nil,
	}
	for i := range f.Services {
		for _, m := range extractService(f, f.Services[i]).Methods {
			w := want[m.Name]
			switch {
			case w == nil && m.MCP != nil:
				t.Errorf("%s.MCP = %+v, want nil", m.Name, *m.MCP)
			case w != nil && (m.MCP == nil || *m.MCP != *w):
				t.Errorf("%s.MCP = %+v, want %+v", m.Name, m.MCP, *w)
			}
			if m.Name == "ListOrders" && m.Description != "ListOrders returns the caller's orders.\nNewest first." {
				t.Errorf("ListOrders.Description = %q", m.Description)
			}
		}
	}
}
