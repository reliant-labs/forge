package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProto is a test helper that writes a proto file and returns its path.
func writeProto(t *testing.T, name, contents string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

func TestParseProtoFileKeepsServiceOpenAcrossOptionBlocks(t *testing.T) {
	path := writeProto(t, "service.proto", `syntax = "proto3";

package services.api.v1;

option go_package = "example.com/test/gen/services/api/v1;apiv1";

service ApiService {
  option (forge.options.v1.service_options) = {
    name: "ApiService"
    version: "1.0.0"
    description: "API service"
  };

  rpc Create(CreateRequest) returns (CreateResponse) {
    option (forge.options.v1.method_options) = {
      auth_required: true
    };
  }

  rpc Get(GetRequest) returns (GetResponse) {
    option (forge.options.v1.method_options) = {
      auth_required: true
      cache: { key_template: "Api:{request.id}" }
    };
  }
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	if got := len(services[0].Methods); got != 2 {
		t.Fatalf("expected 2 methods, got %d", got)
	}
	if services[0].Methods[0].Name != "Create" || services[0].Methods[1].Name != "Get" {
		t.Fatalf("unexpected methods parsed: %+v", services[0].Methods)
	}
}

func TestParseProtoFile_BlockComments(t *testing.T) {
	path := writeProto(t, "comments.proto", `syntax = "proto3";

package test.v1;

option go_package = "example.com/test/gen/test/v1;testv1";

/* This is a block comment
   spanning multiple lines */
service CommentService {
  /* another block comment */
  rpc DoStuff(DoStuffRequest) returns (DoStuffResponse);

  /*
   * Nested-looking block comment
   * with asterisks
   */
  rpc DoMore(DoMoreRequest) returns (DoMoreResponse);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	if got := len(services[0].Methods); got != 2 {
		t.Fatalf("expected 2 methods, got %d", got)
	}
	if services[0].Methods[0].Name != "DoStuff" {
		t.Errorf("expected method DoStuff, got %s", services[0].Methods[0].Name)
	}
	if services[0].Methods[1].Name != "DoMore" {
		t.Errorf("expected method DoMore, got %s", services[0].Methods[1].Name)
	}
}

func TestParseProtoFile_MultipleServices(t *testing.T) {
	path := writeProto(t, "multi.proto", `syntax = "proto3";

package multi.v1;

option go_package = "example.com/test/gen/multi/v1;multiv1";

service Alpha {
  rpc One(OneReq) returns (OneResp);
}

service Beta {
  rpc Two(TwoReq) returns (TwoResp);
  rpc Three(ThreeReq) returns (ThreeResp);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(services))
	}
	if services[0].Name != "Alpha" {
		t.Errorf("expected Alpha, got %s", services[0].Name)
	}
	if len(services[0].Methods) != 1 {
		t.Errorf("expected 1 method on Alpha, got %d", len(services[0].Methods))
	}
	if services[1].Name != "Beta" {
		t.Errorf("expected Beta, got %s", services[1].Name)
	}
	if len(services[1].Methods) != 2 {
		t.Errorf("expected 2 methods on Beta, got %d", len(services[1].Methods))
	}
}

func TestParseProtoFile_NoServices(t *testing.T) {
	path := writeProto(t, "messages_only.proto", `syntax = "proto3";

package msgs.v1;

option go_package = "example.com/test/gen/msgs/v1;msgsv1";

message Foo {
  string bar = 1;
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 0 {
		t.Fatalf("expected 0 services, got %d", len(services))
	}
}

func TestParseProtoFile_StreamingRPCs(t *testing.T) {
	path := writeProto(t, "streaming.proto", `syntax = "proto3";

package stream.v1;

option go_package = "example.com/test/gen/stream/v1;streamv1";

service StreamService {
  rpc ServerStream(Req) returns (stream Resp);
  rpc ClientStream(stream Req) returns (Resp);
  rpc BidiStream(stream Req) returns (stream Resp);
  rpc Unary(Req) returns (Resp);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	methods := services[0].Methods
	if len(methods) != 4 {
		t.Fatalf("expected 4 methods, got %d", len(methods))
	}

	// ServerStream: server streaming only
	if methods[0].ClientStreaming || !methods[0].ServerStreaming {
		t.Errorf("ServerStream: expected server-streaming, got client=%v server=%v",
			methods[0].ClientStreaming, methods[0].ServerStreaming)
	}
	// ClientStream: client streaming only
	if !methods[1].ClientStreaming || methods[1].ServerStreaming {
		t.Errorf("ClientStream: expected client-streaming, got client=%v server=%v",
			methods[1].ClientStreaming, methods[1].ServerStreaming)
	}
	// BidiStream: both
	if !methods[2].ClientStreaming || !methods[2].ServerStreaming {
		t.Errorf("BidiStream: expected bidi streaming, got client=%v server=%v",
			methods[2].ClientStreaming, methods[2].ServerStreaming)
	}
	// Unary: neither
	if methods[3].ClientStreaming || methods[3].ServerStreaming {
		t.Errorf("Unary: expected no streaming, got client=%v server=%v",
			methods[3].ClientStreaming, methods[3].ServerStreaming)
	}
}

func TestParseProtoFile_GoPackageWithoutAlias(t *testing.T) {
	path := writeProto(t, "noalias.proto", `syntax = "proto3";

package noalias.v1;

option go_package = "example.com/test/gen/noalias/v1";

service NoAliasService {
  rpc Ping(PingReq) returns (PingResp);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	svc := services[0]
	if svc.GoPackage != "example.com/test/gen/noalias/v1" {
		t.Errorf("GoPackage = %q, want %q", svc.GoPackage, "example.com/test/gen/noalias/v1")
	}
	if svc.PkgName != "v1" {
		t.Errorf("PkgName = %q, want %q", svc.PkgName, "v1")
	}
}

func TestParseProtoFile_GoPackageWithAlias(t *testing.T) {
	path := writeProto(t, "alias.proto", `syntax = "proto3";

package alias.v1;

option go_package = "example.com/test/gen/alias/v1;aliasv1";

service AliasService {
  rpc Ping(PingReq) returns (PingResp);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	svc := services[0]
	if svc.GoPackage != "example.com/test/gen/alias/v1" {
		t.Errorf("GoPackage = %q, want %q", svc.GoPackage, "example.com/test/gen/alias/v1")
	}
	if svc.PkgName != "aliasv1" {
		t.Errorf("PkgName = %q, want %q", svc.PkgName, "aliasv1")
	}
}

func TestParseProtoFile_FullyQualifiedTypes(t *testing.T) {
	path := writeProto(t, "fqn.proto", `syntax = "proto3";

package fqn.v1;

option go_package = "example.com/test/gen/fqn/v1;fqnv1";

service FQNService {
  rpc Do(google.protobuf.Empty) returns (google.protobuf.Empty);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	m := services[0].Methods[0]
	if m.InputType != "google.protobuf.Empty" {
		t.Errorf("InputType = %q, want google.protobuf.Empty", m.InputType)
	}
	if m.OutputType != "google.protobuf.Empty" {
		t.Errorf("OutputType = %q, want google.protobuf.Empty", m.OutputType)
	}
	if !m.IsInputEmpty() || !m.IsOutputEmpty() {
		t.Errorf("expected IsInputEmpty/IsOutputEmpty to be true")
	}
}

func TestParseProtoFile_PackageAndModulePath(t *testing.T) {
	path := writeProto(t, "meta.proto", `syntax = "proto3";

package meta.v1;

option go_package = "example.com/mymod/gen/meta/v1;metav1";

service MetaService {
  rpc Do(DoReq) returns (DoResp);
}
`)

	services, err := parseProtoFile(path, "example.com/mymod")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	svc := services[0]
	if svc.Package != "meta.v1" {
		t.Errorf("Package = %q, want meta.v1", svc.Package)
	}
	if svc.ModulePath != "example.com/mymod" {
		t.Errorf("ModulePath = %q, want example.com/mymod", svc.ModulePath)
	}
	if svc.ProtoFile != path {
		t.Errorf("ProtoFile = %q, want %q", svc.ProtoFile, path)
	}
}

func TestParseProtoFile_AuthRequired(t *testing.T) {
	path := writeProto(t, "auth.proto", `syntax = "proto3";

package auth.v1;

option go_package = "example.com/test/gen/auth/v1;authv1";

service AuthService {
  rpc Create(CreateReq) returns (CreateResp) {
    option (forge.options.v1.method_options) = {
      auth_required: true
    };
  }
  rpc Public(PublicReq) returns (PublicResp) {
    option (forge.options.v1.method_options) = {
      auth_required: false
    };
  }
  rpc NoOption(NoOptReq) returns (NoOptResp);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	methods := services[0].Methods
	if len(methods) != 3 {
		t.Fatalf("expected 3 methods, got %d", len(methods))
	}
	if !methods[0].AuthRequired {
		t.Errorf("Create: expected AuthRequired=true, got false")
	}
	if methods[1].AuthRequired {
		t.Errorf("Public: expected AuthRequired=false, got true")
	}
	if methods[2].AuthRequired {
		t.Errorf("NoOption: expected AuthRequired=false (default), got true")
	}
}

func TestParseProtoFile_RequiredRoles(t *testing.T) {
	path := writeProto(t, "roles.proto", `syntax = "proto3";

package roles.v1;

option go_package = "example.com/test/gen/roles/v1;rolesv1";

service RolesService {
  rpc AdminOnly(AdminReq) returns (AdminResp) {
    option (forge.options.v1.method_options) = {
      auth_required: true
      required_roles: ["admin", "org:admin"]
    };
  }
  rpc AnyAuth(AuthReq) returns (AuthResp) {
    option (forge.options.v1.method_options) = {
      auth_required: true
    };
  }
  rpc Public(PubReq) returns (PubResp) {
    option (forge.options.v1.method_options) = {
      auth_required: false
    };
  }
  rpc NoOption(NoOptReq) returns (NoOptResp);
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	methods := services[0].Methods
	if len(methods) != 4 {
		t.Fatalf("expected 4 methods, got %d", len(methods))
	}

	// AdminOnly: auth_required=true, required_roles=["admin", "org:admin"]
	if !methods[0].AuthRequired {
		t.Errorf("AdminOnly: expected AuthRequired=true, got false")
	}
	if len(methods[0].RequiredRoles) != 2 {
		t.Fatalf("AdminOnly: expected 2 required_roles, got %d: %v", len(methods[0].RequiredRoles), methods[0].RequiredRoles)
	}
	if methods[0].RequiredRoles[0] != "admin" || methods[0].RequiredRoles[1] != "org:admin" {
		t.Errorf("AdminOnly: expected required_roles=[admin, org:admin], got %v", methods[0].RequiredRoles)
	}

	// AnyAuth: auth_required=true, no roles
	if !methods[1].AuthRequired {
		t.Errorf("AnyAuth: expected AuthRequired=true, got false")
	}
	if len(methods[1].RequiredRoles) != 0 {
		t.Errorf("AnyAuth: expected 0 required_roles, got %d", len(methods[1].RequiredRoles))
	}

	// Public: auth_required=false, no roles
	if methods[2].AuthRequired {
		t.Errorf("Public: expected AuthRequired=false, got true")
	}
	if len(methods[2].RequiredRoles) != 0 {
		t.Errorf("Public: expected 0 required_roles, got %d", len(methods[2].RequiredRoles))
	}

	// NoOption: defaults
	if methods[3].AuthRequired {
		t.Errorf("NoOption: expected AuthRequired=false (default), got true")
	}
	if len(methods[3].RequiredRoles) != 0 {
		t.Errorf("NoOption: expected 0 required_roles, got %d", len(methods[3].RequiredRoles))
	}
}

func TestParseProtoFile_RequiredRolesSingleValue(t *testing.T) {
	path := writeProto(t, "single_role.proto", `syntax = "proto3";

package single.v1;

option go_package = "example.com/test/gen/single/v1;singlev1";

service SingleService {
  rpc Do(DoReq) returns (DoResp) {
    option (forge.options.v1.method_options) = {
      auth_required: true
      required_roles: "admin"
    };
  }
}
`)

	services, err := parseProtoFile(path, "example.com/test")
	if err != nil {
		t.Fatalf("parseProtoFile() error = %v", err)
	}
	methods := services[0].Methods
	if len(methods[0].RequiredRoles) != 1 {
		t.Fatalf("expected 1 required_role, got %d: %v", len(methods[0].RequiredRoles), methods[0].RequiredRoles)
	}
	if methods[0].RequiredRoles[0] != "admin" {
		t.Errorf("expected required_roles=[admin], got %v", methods[0].RequiredRoles)
	}
}

func TestParseProtoFile_MissingGoPackageWithService(t *testing.T) {
	path := writeProto(t, "nopkg.proto", `syntax = "proto3";

package nopkg.v1;

service Broken {
  rpc Do(Req) returns (Resp);
}
`)

	_, err := parseProtoFile(path, "example.com/test")
	if err == nil {
		t.Fatal("expected error for missing go_package with service, got nil")
	}
}