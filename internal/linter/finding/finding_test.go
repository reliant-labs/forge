package finding

import (
	"encoding/json"
	"testing"
)

func TestParseSeverity(t *testing.T) {
	tests := []struct {
		in     string
		want   Severity
		wantOK bool
	}{
		{"error", SeverityError, true},
		{"ERROR", SeverityError, true},
		{"  Error  ", SeverityError, true},
		{"warning", SeverityWarning, true},
		{"warn", SeverityWarning, true}, // forge.yaml's own spelling
		{"WARN", SeverityWarning, true},
		{"info", SeverityInfo, true},
		{"off", "", false},
		{"", "", false},
		{"fatal", "", false},
		{"warnings", "", false},
	}
	for _, tt := range tests {
		got, ok := ParseSeverity(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("ParseSeverity(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// The JSON contract spells each level one way; a drift here is the bug the
// package exists to prevent.
func TestSeverityWireSpelling(t *testing.T) {
	for sev, want := range map[Severity]string{SeverityError: "error", SeverityWarning: "warning", SeverityInfo: "info"} {
		if string(sev) != want {
			t.Errorf("%v spells %q, want %q", sev, string(sev), want)
		}
	}
}

func TestResultHasErrors(t *testing.T) {
	tests := []struct {
		name string
		r    Result
		want bool
	}{
		{"no findings", Result{}, false},
		{"only warnings and infos never gate", Result{Findings: []Finding{{Severity: SeverityWarning}, {Severity: SeverityInfo}}}, false},
		{"one error among warnings gates", Result{Findings: []Finding{{Severity: SeverityWarning}, {Severity: SeverityError}}}, true},
		{"an unrecognised severity does not gate", Result{Findings: []Finding{{Severity: "fatal"}}}, false},
	}
	for _, tt := range tests {
		if got := tt.r.HasErrors(); got != tt.want {
			t.Errorf("%s: HasErrors = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestFindingJSONShape(t *testing.T) {
	full, err := json.Marshal(Finding{Rule: "r", Severity: SeverityError, File: "a.go", Line: 3, Message: "m", Path: "p", Remediation: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"rule":"r","severity":"error","file":"a.go","line":3,"message":"m","path":"p","remediation":"fix"}`
	if string(full) != want {
		t.Errorf("full finding = %s, want %s", full, want)
	}
	// Fields a linter leaves zero are omitted, so each linter's historical wire shape is intact.
	sparse, _ := json.Marshal(Finding{Rule: "r", Severity: SeverityWarning, Message: "m"})
	if string(sparse) != `{"rule":"r","severity":"warning","message":"m"}` {
		t.Errorf("sparse finding = %s", sparse)
	}
	var round Result
	if err := json.Unmarshal([]byte(`{"findings":[{"rule":"x","severity":"info","message":"hello"}]}`), &round); err != nil || len(round.Findings) != 1 || round.Findings[0].Severity != SeverityInfo {
		t.Errorf("round trip = (%+v, %v)", round, err)
	}
}
