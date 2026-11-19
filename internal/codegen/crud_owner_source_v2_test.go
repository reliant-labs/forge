package codegen

import (
	"go/parser"
	"go/token"
	"testing"
)

// connect-go v2 CRUD handlers take the bare request message. The owner scan
// reads them like v1's, and still skips a helper whose message is not the
// method's own.
func TestScanCRUDOwnerSources_ConnectV2Shapes(t *testing.T) {
	const src = `package a

import (
	"context"

	tn "example.com/app/internal/tenancy"
	"github.com/reliant-labs/forge/pkg/orm"

	pb "example.com/app/gen/services/a/v1"
)

func (s *Service) GetA(ctx context.Context, req *pb.GetARequest) (*pb.GetAResponse, error) {
	tid, err := tn.TenantID(ctx)
	if err != nil {
		return nil, err
	}
	_ = orm.WhereEq("tenant_id", tid)
	return nil, nil
}

// load takes another method's request: a helper, not an RPC.
func (s *Service) load(ctx context.Context, req *pb.GetARequest) error {
	owner, err := tn.UserID(ctx)
	if err != nil {
		return err
	}
	_ = orm.WhereEq("tenant_id", owner)
	return nil
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	uses := ScanCRUDOwnerSources(fset, f)
	if len(uses) != 1 {
		t.Fatalf("want 1 use (GetA), got %d: %+v", len(uses), uses)
	}
	get := uses[0]
	if get.Method != "GetA" || get.Column != "tenant_id" || !get.Read ||
		get.Source.Kind != CRUDOwnerFromResolver || get.Source.Func != "tn.TenantID" {
		t.Errorf("GetA (v2): resolver not traced: %+v", get)
	}
}
