package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rpc named List<X>s is CRUD whenever a message <X> exists — a plain
// response element included. `ListDueReads` with a `DueRead` element (no
// table, no forge:entity) failed phase 2 of `forge scaffold` at the CRUD
// step, ~9 minutes into a fresh project, while `--dry-run` had predicted a
// clean run. The classification is pure proto + schema analysis, so the
// sweep now answers it before phase 1, in both modes.

const dueReadsRPCs = `  rpc TailEvents(TailEventsRequest) returns (stream TailEventsResponse);
  rpc ListDueReads(ListDueReadsRequest) returns (ListDueReadsResponse);
  // Over a marked entity this same run births: its table is what the
  // birth writes, so it is CRUD and matched.
  rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);
  // Over an applied table.
  rpc ListLegacies(ListLegaciesRequest) returns (ListLegaciesResponse);`

const dueReadsMessages = `
message DueRead {
  string channel = 1;
  int64 due_in_seconds = 2;
}
message ListDueReadsRequest { int32 page_size = 1; }
message ListDueReadsResponse { repeated DueRead due_reads = 1; }
message GetOrderRequest { string id = 1; }
message GetOrderResponse { Order order = 1; }
message ListLegaciesRequest { int32 page_size = 1; }
message ListLegaciesResponse { repeated Legacy legacies = 1; }
`

func setupDueReadsFixture(t *testing.T, custom bool) string {
	t.Helper()
	dir := setupProjectScaffoldFixture(t)
	rpcs := dueReadsRPCs
	if custom {
		rpcs = strings.Replace(rpcs, "  rpc ListDueReads(", "  // forge:custom\n  rpc ListDueReads(", 1)
	}
	proto := strings.Replace(scaffoldFixtureProto,
		"  rpc TailEvents(TailEventsRequest) returns (stream TailEventsResponse);", rpcs, 1) + dueReadsMessages
	writeFixtureFile(t, dir, filepath.Join("proto", "services", "tasks", "v1", "tasks.proto"), proto)
	return dir
}

func TestProjectScaffold_UnmatchedCRUDRPCFailsBeforeAnythingIsWritten(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		name := "real run"
		if dryRun {
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			dir := setupDueReadsFixture(t, false)
			protoPath := filepath.Join(dir, "proto", "services", "tasks", "v1", "tasks.proto")
			protoBefore := readFileT(t, protoPath)

			runs := 0
			f := testFactory()
			f.Gen.RunPipeline = func(string) error { runs++; return nil }
			out, err := captureStdout(t, func() error { return runSweep(f, "", dryRun) })
			if err == nil {
				t.Fatalf("the sweep must refuse ListDueReads (entity DueRead has a wire message and no table); it passed:\n%s", out)
			}
			msg := err.Error()
			for _, want := range []string{
				"TasksService.ListDueReads: entity DueRead, no table due_reads or due_read",
				"// forge:custom",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal must name %q; got:\n%s", want, msg)
				}
			}
			// The matched rpcs are not listed: one over a marked entity this
			// run births, one over an applied table.
			for _, never := range []string{"GetOrder", "ListLegacies"} {
				if strings.Contains(msg, never) {
					t.Errorf("%s has a table (born this run, or applied) and must not be refused:\n%s", never, msg)
				}
			}
			if runs != 0 {
				t.Errorf("the refusal must come before phase 2; the pipeline ran %d time(s)", runs)
			}
			if got := readFileT(t, protoPath); got != protoBefore {
				t.Error("the refusal must come before phase 1; the proto was modified")
			}
			entries, _ := os.ReadDir(filepath.Join(dir, "db", "migrations"))
			if len(entries) != 1 { // the fixture's one pre-existing migration
				t.Errorf("the refusal must come before phase 1; db/migrations has %d entries", len(entries))
			}
		})
	}
}

func TestProjectScaffold_CustomMarkedListRPCPassesThePreflight(t *testing.T) {
	setupDueReadsFixture(t, true)
	f := testFactory()
	f.Gen.RunPipeline = func(string) error { return nil }
	out, err := captureStdout(t, func() error { return runSweep(f, "", true) })
	if err != nil {
		t.Fatalf("a `// forge:custom` ListDueReads is not CRUD; the dry run must pass: %v\n%s", err, out)
	}
}
