package mlcollect

import (
	"reflect"
	"strings"
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
		// Scripts quoted inside a script are redacted by the same rules.
		{[]string{"sh", "-c", `bash -lc 'agent --token inner-secret --model m'`},
			[]string{"sh", "-c", `bash -lc 'agent --token REDACTED --model m'`}},
		{[]string{"sh", "-c", `sh -c "agent --password pw-secret"; bash -lc "agent --token=eq-secret"`},
			[]string{"sh", "-c", `sh -c 'agent --password REDACTED'; bash -lc 'agent --token=REDACTED'`}},
		// An attached credential takes its whole value, spaces and all.
		{[]string{"--token=a b"}, []string{"--token=REDACTED"}},
		{[]string{"sh", "-c", `agent --token="a b" --api-key='c d'`}, []string{"sh", "-c", "agent --token=REDACTED --api-key=REDACTED"}},
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

// TestRedactArgsNestedScripts: a credential survives no depth of scripts
// quoted inside scripts, and what discovery reads survives the quoting.
func TestRedactArgsNestedScripts(t *testing.T) {
	deep := "agent --token deep-secret --model m"
	for range 12 { // deeper than maxScriptDepth
		deep = "sh -c '" + strings.ReplaceAll(deep, "'", `'\''`) + "'"
	}
	for _, argv := range [][]string{
		{"sh", "-c", `sh -c "bash -c 'agent --api-key \"k1 k2-secret\" --model m'"`},
		{"bash", "-lc", `exec sh -c 'agent --password pw-secret --model m'`},
		{"sh", "-c", `sh -c "agent --token=eq-secret --model m"`},
		{"sh", "-c", `sh -c "agent --token=\"eq x-secret\" --model m"`},
		{"sh", "-c", deep},
	} {
		got := strings.Join(RedactArgs(argv), " ")
		for _, secret := range []string{"k1", "k2-secret", "pw-secret", "eq-secret", "x-secret", "deep-secret"} {
			if strings.Contains(got, secret) {
				t.Errorf("RedactArgs(%q) = %q keeps %q", argv, got, secret)
			}
		}
		if argv[2] != deep && !strings.Contains(got, "--model m") {
			t.Errorf("RedactArgs(%q) = %q lost --model", argv, got)
		}
	}
}

// TestRedactObjectKeepsArgvRedactionOfDocuments: a credential flag's value
// that is a document stays redacted whole, in a list and across command and
// args; a document after any other flag is redacted as a document.
func TestRedactObjectKeepsArgvRedactionOfDocuments(t *testing.T) {
	got := RedactObject(map[string]any{
		"spec": map[string]any{
			"list": []any{"--token", "opaque: credential-value\nother: value\n"},
			"pair": map[string]any{"command": []any{"/bin/agent", "--password"}, "args": []any{"opaque: pair-value\n", "--v=2"}},
			"conf": []any{"--config", "receivers:\n  otlp: {}\npassword: conf-value\n"},
		},
	})
	spec := got["spec"].(map[string]any)
	if want := []any{"--token", Redacted}; !reflect.DeepEqual(spec["list"], want) {
		t.Errorf("list: got %q, want %q", spec["list"], want)
	}
	if want := map[string]any{"command": []any{"/bin/agent", "--password"}, "args": []any{Redacted, "--v=2"}}; !reflect.DeepEqual(spec["pair"], want) {
		t.Errorf("pair: got %q, want %q", spec["pair"], want)
	}
	conf := spec["conf"].([]any)[1].(string)
	if strings.Contains(conf, "conf-value") || !strings.Contains(conf, "otlp") {
		t.Errorf("conf: got %q", conf)
	}
}
