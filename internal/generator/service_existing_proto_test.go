package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenerateServiceFiles_UsesTheProjectsExistingProto pins obs 1bc2d82d: in
// reliant every service proto lives in proto/reliant/v1 (package reliant.v1).
// `scaffold service pull_request_record --resume` with
// proto/reliant/v1/pull_request_record.proto present wrote a second stub under
// proto/services/ and a service.go importing gen/services/...; both had to be
// undone by hand.
func TestGenerateServiceFiles_UsesTheProjectsExistingProto(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proto", "reliant", "v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "syntax = \"proto3\";\n\npackage reliant.v1;\n\noption go_package = \"example.com/reliant/gen/reliant/v1;reliantv1\";\n\nservice PullRequestRecordService {\n  rpc Get(GetRequest) returns (GetResponse);\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "pull_request_record.proto"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := GenerateServiceFilesWithMode(root, "example.com/reliant", "pull_request_record", "reliant", ScaffoldResume, nil); err != nil {
		t.Fatalf("GenerateServiceFilesWithMode: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "proto", "services")); !os.IsNotExist(err) {
		t.Fatalf("a second stub tree was written under proto/services (stat err %v)", err)
	}
	svc, err := os.ReadFile(filepath.Join(root, "internal", "handlers", "pull_request_record", "service.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"example.com/reliant/gen/reliant/v1"`,
		`reliantv1connect "example.com/reliant/gen/reliant/v1/reliantv1connect"`,
		`pb.File_reliant_v1_pull_request_record_proto`,
	} {
		if !strings.Contains(string(svc), want) {
			t.Errorf("service.go lacks %s:\n%s", want, svc)
		}
	}
	if strings.Contains(string(svc), "gen/services/") {
		t.Errorf("service.go still imports gen/services/:\n%s", svc)
	}
}

// A declaration whose go_package the template cannot import is refused by
// name, not answered with a duplicate stub.
func TestGenerateServiceFiles_ExistingProtoWithForeignGoPackage(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "proto", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "syntax = \"proto3\";\npackage api;\noption go_package = \"example.com/other/apipb\";\nservice BillingService {}\n"
	if err := os.WriteFile(filepath.Join(dir, "billing.proto"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	err := GenerateServiceFiles(root, "example.com/myapp", "billing", "myapp")
	if err == nil || !strings.Contains(err.Error(), "proto/api/billing.proto declares service BillingService") {
		t.Fatalf("err = %v; want a refusal naming the existing file", err)
	}
}
