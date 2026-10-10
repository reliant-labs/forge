package serverkit

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// TestNewLogger_UserErrorsDoNotLogAtError pins the working default: the
// process logger serverkit builds classifies every record, so a hand-written
// logger.Error carrying a user error is not an ERROR line — not only the
// records forge's own interceptors write.
//
// Not parallel: the handler writes to os.Stdout, which the test swaps.
func TestNewLogger_UserErrorsDoNotLogAtError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	logger := newLogger(Config{LogLevel: slog.LevelInfo})
	os.Stdout = stdout

	logger.Error("charge rejected", "error", svcerr.InsufficientBalance("wallet empty"))
	logger.Error("charge failed", "error", svcerr.Internal("stripe 500"))
	_ = w.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, r); err != nil {
		t.Fatal(err)
	}

	levels := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		levels[rec["msg"].(string)] = rec["level"].(string) + "/" + rec["error_class"].(string)
	}
	if got := levels["charge rejected"]; got != "INFO/user" {
		t.Errorf("user error = %s, want INFO/user", got)
	}
	if got := levels["charge failed"]; got != "ERROR/server" {
		t.Errorf("server error = %s, want ERROR/server", got)
	}
}
