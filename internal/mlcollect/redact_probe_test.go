package mlcollect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

func TestRedactText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in   string
		keep, gone []string
		want       string
	}{
		{name: "yaml collector config",
			in:   "receivers:\n  otlp: {}\nexporters:\n  otlphttp:\n    endpoint: https://x\n    headers:\n      x-api-token: abc\n    auth:\n      authenticator: basicauth\nextensions:\n  basicauth:\n    client_auth:\n      username: u\n      password: hunter2\n",
			keep: []string{"otlp", "endpoint: https://x", "x-api-token: REDACTED", "authenticator: REDACTED", "username: REDACTED", "password: REDACTED"},
			gone: []string{"abc", "hunter2", "basicauth\n    client"}},
		{name: "npd monitor json",
			in:   `{"plugin":"kmsg","rules":[{"type":"permanent","condition":"KernelDeadlock","reason":"NVRMXid","pattern":"NVRM: Xid.*"}],"token":"t0k"}`,
			keep: []string{`"pattern": "NVRM: Xid.*"`, `"reason": "NVRMXid"`, `"token": "REDACTED"`},
			gone: []string{"t0k"}},
		{name: "kubelet config with nested yaml string",
			in:   "kubelet: |\n  apiVersion: kubelet.config.k8s.io/v1beta1\n  kind: KubeletConfiguration\n  tracing:\n    endpoint: otel:4317\n  authentication:\n    x509:\n      clientCAFile: /etc/ca.crt\n",
			keep: []string{"endpoint: otel:4317", "KubeletConfiguration", "clientCAFile: REDACTED"},
			gone: []string{"/etc/ca.crt"}},
		{name: "dcgm csv", in: "# comment\nDCGM_FI_DEV_GPU_UTIL, gauge, util\n\nDCGM_FI_DEV_XID_ERRORS, gauge, xid\n",
			want: "DCGM_FI_DEV_GPU_UTIL, gauge, util\nDCGM_FI_DEV_XID_ERRORS, gauge, xid\n"},
		{name: "script", in: "#!/bin/sh\nexport TOKEN=abc\n", want: Dropped},
		// The PEM armour is split so the fixture (a four-character, invalid body) never reads as key material to a secret scanner.
		{name: "pem", in: "-----BEGIN " + "PRIVATE KEY-----\nMIIE\n-----END " + "PRIVATE KEY-----\n", want: Dropped},
		{name: "bare string", in: "just some text", want: Dropped},
		{name: "empty", in: "", want: ""},
	} {
		got := RedactText(tc.in)
		if tc.want != "" || tc.in == "" {
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
			continue
		}
		for _, k := range tc.keep {
			if !strings.Contains(got, k) {
				t.Errorf("%s: %q missing from\n%s", tc.name, k, got)
			}
		}
		for _, g := range tc.gone {
			if strings.Contains(got, g) {
				t.Errorf("%s: %q kept in\n%s", tc.name, g, got)
			}
		}
	}
}

func TestRedactConfigMap(t *testing.T) {
	t.Parallel()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Annotations: map[string]string{"a": "b"}, ManagedFields: managed},
		Data:       map[string]string{"api-key": "plain", "config.yaml": "a: 1\n"},
		BinaryData: map[string][]byte{"blob": []byte{1, 2}},
	}
	RedactConfigMap(cm)
	if cm.Annotations != nil || cm.ManagedFields != nil || cm.BinaryData != nil {
		t.Errorf("metadata or binaryData kept: %+v", cm)
	}
	if cm.Data["api-key"] != Redacted || cm.Data["blob"] != Dropped || cm.Data["config.yaml"] != "a: 1\n" {
		t.Errorf("data = %v", cm.Data)
	}
}

const sampleMetrics = `# HELP DCGM_FI_DEV_GPU_UTIL GPU utilization (in %).
# TYPE DCGM_FI_DEV_GPU_UTIL gauge
DCGM_FI_DEV_GPU_UTIL{gpu="0",UUID="GPU-1",Hostname="n",modelName="Tesla-K80",pod="p",namespace="ns",container="c"} 23
DCGM_FI_DEV_FB_USED{gpu="0",UUID="GPU-1"} 1024
this line is not a sample
http_request_duration_seconds_bucket{le="0.1",path="/a\"b"} 3
http_request_duration_seconds_sum 1.5
# TYPE http_request_duration_seconds histogram
`

func TestParseExposition(t *testing.T) {
	t.Parallel()
	fam, keys, err := ParseExposition(strings.NewReader(sampleMetrics))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fam, ","); got != "DCGM_FI_DEV_FB_USED,DCGM_FI_DEV_GPU_UTIL,http_request_duration_seconds" {
		t.Errorf("families = %s", got)
	}
	if got := strings.Join(keys, ","); got != "Hostname,UUID,container,gpu,modelName,namespace,path,pod" {
		t.Errorf("label keys = %s", got)
	}
}

// Probe records one result per target; a failure, a timeout and an oversize
// body are errors on that target, never fatal.
func TestProbe(t *testing.T) {
	t.Parallel()
	targets := []mldiscover.ProbeTarget{
		{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "ok", Port: 9400, Path: "/metrics"},
		{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "refused", Port: 9400},
		{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "slow", Port: 9400},
		{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "huge", Port: 9400},
		{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "html", Port: 9400},
	}
	fetch := func(ctx context.Context, tg mldiscover.ProbeTarget) (io.ReadCloser, error) {
		switch tg.Pod {
		case "ok":
			return io.NopCloser(strings.NewReader(sampleMetrics)), nil
		case "refused":
			return nil, errors.New(`pods "refused" is forbidden: cannot get resource "pods/proxy"`)
		case "slow":
			<-ctx.Done()
			return nil, ctx.Err()
		case "huge":
			return io.NopCloser(strings.NewReader(strings.Repeat("DCGM_FI_DEV_GPU_UTIL 1\n", 100))), nil
		}
		return io.NopCloser(strings.NewReader("<html>nope</html>")), nil
	}
	got := Probe(context.Background(), fetch, targets, ProbeOptions{Timeout: 50 * time.Millisecond, MaxBytes: 1000})
	if len(got) != 5 {
		t.Fatalf("probes = %d", len(got))
	}
	if got[0].Error != "" || len(got[0].Families) != 3 || got[0].Path != "/metrics" {
		t.Errorf("ok: %+v", got[0])
	}
	if got[1].Path != "/metrics" || !strings.Contains(got[1].Error, "forbidden") {
		t.Errorf("refused: %+v", got[1])
	}
	if !strings.Contains(got[2].Error, "deadline") {
		t.Errorf("slow: %+v", got[2])
	}
	if !strings.Contains(got[3].Error, "exceeds 1000 bytes") {
		t.Errorf("huge: %+v", got[3])
	}
	if !strings.Contains(got[4].Error, "no metrics") {
		t.Errorf("html: %+v", got[4])
	}
}

// ProxyFetch goes through the API server's pod proxy, GET only.
func TestProxyFetchUsesThePodProxy(t *testing.T) {
	t.Parallel()
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path
		fmt.Fprint(w, sampleMetrics)
	}))
	defer srv.Close()
	kube, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got := Probe(context.Background(), ProxyFetch(kube), []mldiscover.ProbeTarget{
		{Kind: "dcgm-exporter", Namespace: "gpu-operator", Pod: "nvidia-dcgm-exporter-x", Port: 9400, Path: "/metrics"},
	}, ProbeOptions{})
	if seen != "GET /api/v1/namespaces/gpu-operator/pods/nvidia-dcgm-exporter-x:9400/proxy/metrics" {
		t.Errorf("request = %q", seen)
	}
	if got[0].Error != "" || len(got[0].Families) != 3 {
		t.Errorf("probe = %+v", got[0])
	}
}
