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
			[]string{"sh", "-c", `export HF_TOKEN=REDACTED; curl -H "Authorization: REDACTED" https://user:REDACTED@models.example/llama -o /m`}},
		{[]string{"aws", "s3", "cp", "s3://models/llama", "/models"}, []string{"aws", "s3", "cp", "s3://models/llama", "/models"}},
		{[]string{"--url=https://b.s3.amazonaws.com/w?X-Amz-Credential=AKIA&X-Amz-Signature=deadbeef&x=1"},
			[]string{"--url=https://b.s3.amazonaws.com/w?X-Amz-Credential=REDACTED&X-Amz-Signature=REDACTED&x=1"}},
		{[]string{"run", "/etc/alloy/config.alloy", "--storage.path=/tmp/alloy"}, []string{"run", "/etc/alloy/config.alloy", "--storage.path=/tmp/alloy"}},
		{[]string{"/usr/bin/dcgm-exporter", "-f", "/etc/dcgm-exporter/dcgm-metrics.csv", "--kubernetes=true"},
			[]string{"/usr/bin/dcgm-exporter", "-f", "/etc/dcgm-exporter/dcgm-metrics.csv", "--kubernetes=true"}},
		{[]string{"sh", "-c", "run --secret 's3 cr3t' --db-password pw"}, []string{"sh", "-c", "run --secret REDACTED --db-password REDACTED"}},
		{nil, nil},
	} {
		if got := RedactArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("RedactArgs(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
