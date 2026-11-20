package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/reliant-labs/forge/pkg/authn"
)

// TestTraceHandlerNamesTheUser pins NS FG-10: a record logged inside an
// authenticated request carries user.id, the key an issue's affected-users
// count reads; a record that names a user itself keeps its own.
func TestTraceHandlerNamesTheUser(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(NewTraceHandler(slog.NewJSONHandler(&buf, nil)))
	ctx := authn.ContextWithUserID(context.Background(), "u-42")
	log.ErrorContext(ctx, "boom")
	log.InfoContext(ctx, "acting for another", slog.String("user.id", "u-7"))
	log.InfoContext(context.Background(), "anonymous")

	var lines []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	if lines[0]["user.id"] != "u-42" || lines[1]["user.id"] != "u-7" {
		t.Fatalf("user.id = %v / %v, want u-42 / u-7", lines[0]["user.id"], lines[1]["user.id"])
	}
	if _, ok := lines[2]["user.id"]; ok {
		t.Fatalf("a record with no principal named a user: %v", lines[2])
	}
}
