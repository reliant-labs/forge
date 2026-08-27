package doctor

import (
	"testing"
)

// parsePyroscopeLabels must separate "I could not read the answer" from
// "I read it and it is empty". Only the first is UNDETERMINED; reporting
// both as a warning made a hole in the report look like a finding about
// ingestion.
func TestParsePyroscopeLabels(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Status
	}{
		{
			name: "plain array of labels",
			body: `["app","other"]`,
			want: StatusPass,
		},
		{
			name: "grpc-web values wrapper",
			body: `{"values":["app"]}`,
			want: StatusPass,
		},
		{
			name: "grpc-web names wrapper",
			body: `{"names":["app"]}`,
			want: StatusPass,
		},
		{
			// Decoded fine, genuinely empty: a real finding, not a hole.
			name: "decoded but empty is a warning about ingestion",
			body: `[]`,
			want: StatusWarn,
		},
		{
			// In none of the shapes we can decode — Pyroscope may well be
			// ingesting. forge could not obtain the fact.
			name: "undecodable body is undetermined",
			body: `<html>not json</html>`,
			want: StatusUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parsePyroscopeLabels([]byte(tt.body), "app")
			if got.Status != tt.want {
				t.Fatalf("status = %s, want %s (message: %s)", got.Status, tt.want, got.Message)
			}
		})
	}
}
