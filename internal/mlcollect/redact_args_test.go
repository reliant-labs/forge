package mlcollect

import (
	"reflect"
	"testing"
)

func TestRedactArgs(t *testing.T) {
	for _, c := range []struct {
		in, want []string
	}{
		{[]string{"--token=abc123", "--model=meta-llama/Llama-3-8B"}, []string{"--token=REDACTED", "--model=meta-llama/Llama-3-8B"}},
		{[]string{"vllm", "serve", "meta-llama/Llama-3-8B", "--api-key", "sk-live", "--port", "8000"},
			[]string{"vllm", "serve", "meta-llama/Llama-3-8B", "--api-key", "REDACTED", "--port", "8000"}},
		{[]string{"--password", "--verbose"}, []string{"--password", "--verbose"}},
		{[]string{"sh", "-c", `export HF_TOKEN=hf_abc; curl -H "Authorization: Bearer xyz" https://user:pw@models.example/llama -o /m`},
			[]string{"sh", "-c", `export HF_TOKEN=REDACTED; curl -H REDACTED https://user:REDACTED@models.example/llama -o /m`}},
		{[]string{"aws", "s3", "cp", "s3://models/llama", "/models"}, []string{"aws", "s3", "cp", "s3://models/llama", "/models"}},
		{[]string{"--url=https://b.s3.amazonaws.com/w?X-Amz-Credential=AKIA&X-Amz-Signature=deadbeef&x=1"},
			[]string{"--url=https://b.s3.amazonaws.com/w?X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&x=1"}},
		{[]string{"run", "/etc/alloy/config.alloy", "--storage.path=/tmp/alloy"}, []string{"run", "/etc/alloy/config.alloy", "--storage.path=/tmp/alloy"}},
		{[]string{"/usr/bin/dcgm-exporter", "-f", "/etc/dcgm-exporter/dcgm-metrics.csv", "--kubernetes=true"},
			[]string{"/usr/bin/dcgm-exporter", "-f", "/etc/dcgm-exporter/dcgm-metrics.csv", "--kubernetes=true"}},
		{[]string{"sh", "-c", "run --secret 's3 cr3t' --db-password pw"}, []string{"sh", "-c", "run --secret REDACTED --db-password REDACTED"}},
		{[]string{"python", "-m", "vllm.entrypoints.openai.api_server", "--model", "meta-llama/Llama-3-8B"},
			[]string{"python", "-m", "vllm.entrypoints.openai.api_server", "--model", "meta-llama/Llama-3-8B"}},
		// curl's user:password, split and attached, in an argv and a script.
		{[]string{"curl", "-u", "admin:curl-pass", "https://models.example/w"}, []string{"curl", "-u", "REDACTED", "https://models.example/w"}},
		{[]string{"curl", "-uadmin:curl-pass", "--user=admin:curl-pass", "https://x"}, []string{"curl", "-uREDACTED", "--user=REDACTED", "https://x"}},
		{[]string{"sh", "-c", "curl -fsSL -u admin:curl-pass https://m.example/w -o /models/w"},
			[]string{"sh", "-c", "curl -fsSL -u REDACTED https://m.example/w -o /models/w"}},
		// Any header, split from its flag.
		{[]string{"curl", "-H", "X-Tenant-Id: t-123", "--header", "X-Org: o"}, []string{"curl", "-H", "REDACTED", "--header", "REDACTED"}},
		// Short password flags: split, attached and program-specific, but a
		// port, a mapping or a path after -p is kept.
		{[]string{"sshpass", "-p", "ssh-pass", "ssh", "host"}, []string{"sshpass", "-p", "REDACTED", "ssh", "host"}},
		{[]string{"sh", "-c", "mysql -h db -u root -pS3cret -e 'select 1' && redis-cli -a redis-pass ping"},
			[]string{"sh", "-c", "mysql -h db -u root -pREDACTED -e 'select 1' && redis-cli -a REDACTED ping"}},
		{[]string{"openssl", "enc", "-k", "enc-pass", "-passin", "pass:in-pass"}, []string{"openssl", "enc", "-k", "REDACTED", "-passin", "REDACTED"}},
		{[]string{"docker", "run", "-p", "8080:80", "img"}, []string{"docker", "run", "-p", "8080:80", "img"}},
		{[]string{"sh", "-c", "mkdir -p /models && python -u train.py"}, []string{"sh", "-c", "mkdir -p /models && python -u train.py"}},
		// Vendor-format tokens wherever they are.
		{[]string{"--extra", "hf_abcdefghijklmnopqrstuvwxyz0123"}, []string{"--extra", "REDACTED"}},
		{nil, nil},
	} {
		if got := RedactArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("RedactArgs(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestRedactArgvAcrossCommandAndArgs: a flag that ends the command takes
// its value from the args, as the kubelet runs them.
func TestRedactArgvAcrossCommandAndArgs(t *testing.T) {
	cmd, args := RedactArgv([]string{"/app/server", "--token"}, []string{"split-secret", "--port", "8000"})
	if !reflect.DeepEqual(cmd, []string{"/app/server", "--token"}) || !reflect.DeepEqual(args, []string{"REDACTED", "--port", "8000"}) {
		t.Errorf("got %q %q", cmd, args)
	}
	if c, a := RedactArgv(nil, []string{"--model", "m"}); c != nil || !reflect.DeepEqual(a, []string{"--model", "m"}) {
		t.Errorf("args only: got %q %q", c, a)
	}
	if c, a := RedactArgv(nil, nil); c != nil || a != nil {
		t.Errorf("neither: got %q %q", c, a)
	}
}

// TestRedactObjectArgLists: a custom object's string lists are argvs, and a
// container's command and args in one are one argv.
func TestRedactObjectArgLists(t *testing.T) {
	got := RedactObject(map[string]any{
		"spec": map[string]any{
			"dcgmExporter": map[string]any{"args": []any{"--token", "cr-list-secret", "-f", "/etc/dcgm.csv"}},
			"container":    map[string]any{"command": []any{"/bin/agent", "--api-key"}, "args": []any{"cr-split-secret", "--v=2"}},
			"receivers":    []any{"otlp", "prometheus"},
		},
	})
	want := map[string]any{
		"spec": map[string]any{
			"dcgmExporter": map[string]any{"args": []any{"--token", "REDACTED", "-f", "/etc/dcgm.csv"}},
			"container":    map[string]any{"command": []any{"/bin/agent", "--api-key"}, "args": []any{"REDACTED", "--v=2"}},
			"receivers":    []any{"otlp", "prometheus"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
