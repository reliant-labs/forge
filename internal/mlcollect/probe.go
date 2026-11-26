package mlcollect

// The opt-in probe (`forge ml discover --probe`): read an exporter's /metrics
// through the API server's pod proxy and keep only what names coverage —
// metric family names and label keys, never a sample value.
//
// It is the one read outside ClusterRole(): it needs `get pods/proxy`
// (ProbeRules()), which reaches any port of any pod, so it runs only when
// asked. It reads only the targets mldiscover.ProbeTargets names (at most one
// pod per component and node pool), each with a timeout and a size cap, and a
// failed probe is recorded on its MetricsProbe, never fatal.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// The probe's bounds.
const (
	ProbeTimeout  = 10 * time.Second
	ProbeMaxBytes = 8 << 20
)

// Fetch opens one target's metrics body.
type Fetch func(ctx context.Context, t mldiscover.ProbeTarget) (io.ReadCloser, error)

// ProxyFetch reads a target through the API server's pod proxy:
// GET /api/v1/namespaces/{ns}/pods/{pod}:{port}/proxy{path}.
func ProxyFetch(kube kubernetes.Interface) Fetch {
	return func(ctx context.Context, t mldiscover.ProbeTarget) (io.ReadCloser, error) {
		path := t.Path
		if path == "" {
			path = "/metrics"
		}
		return kube.CoreV1().RESTClient().Get().
			Namespace(t.Namespace).Resource("pods").
			Name(fmt.Sprintf("%s:%d", t.Pod, t.Port)).
			SubResource("proxy").Suffix(path).
			Stream(ctx)
	}
}

// ProbeOptions tunes Probe.
type ProbeOptions struct {
	// Timeout and MaxBytes override ProbeTimeout and ProbeMaxBytes (tests).
	Timeout  time.Duration
	MaxBytes int64
}

// Probe reads each target with fetch and returns one MetricsProbe per target,
// in target order.
func Probe(ctx context.Context, fetch Fetch, targets []mldiscover.ProbeTarget, opts ProbeOptions) []mldiscover.MetricsProbe {
	timeout, limit := opts.Timeout, opts.MaxBytes
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	if limit <= 0 {
		limit = ProbeMaxBytes
	}
	out := make([]mldiscover.MetricsProbe, 0, len(targets))
	for _, t := range targets {
		p := mldiscover.MetricsProbe{Kind: t.Kind, Namespace: t.Namespace, Pod: t.Pod, Node: t.Node, Port: t.Port, Path: t.Path}
		if p.Path == "" {
			p.Path = "/metrics"
		}
		p.Families, p.LabelKeys, p.Error = probeOne(ctx, fetch, t, timeout, limit)
		out = append(out, p)
	}
	return out
}

func probeOne(ctx context.Context, fetch Fetch, t mldiscover.ProbeTarget, timeout time.Duration, limit int64) ([]string, []string, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := fetch(ctx, t)
	if err != nil {
		return nil, nil, err.Error()
	}
	defer func() { _ = body.Close() }() // a read-only GET: nothing to flush or report
	lr := &io.LimitedReader{R: body, N: limit + 1}
	families, labels, perr := ParseExposition(lr)
	switch {
	case lr.N <= 0:
		return families, labels, fmt.Sprintf("the response exceeds %d bytes: read stopped there", limit)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return families, labels, fmt.Sprintf("timed out after %s", timeout)
	case perr != nil:
		return families, labels, perr.Error()
	case len(families) == 0:
		return nil, nil, "no metrics in the response (not the Prometheus text format?)"
	}
	return families, labels, ""
}

// ParseExposition reads the Prometheus text exposition format leniently and
// returns the sorted metric family names and label keys. A family is named by
// its `# TYPE`/`# HELP` line or, without one, by its samples; a histogram's or
// summary's _bucket/_sum/_count samples belong to their family. Values are
// never kept. It is a line parser rather than prometheus/common's expfmt on
// purpose: expfmt stops at the first malformed line, and one odd line from a
// third-party exporter must not hide every family after it.
func ParseExposition(r io.Reader) ([]string, []string, error) {
	families := map[string]bool{}
	labels := map[string]bool{}
	var samples []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			f := strings.Fields(line)
			if len(f) >= 3 && (f[1] == "TYPE" || f[1] == "HELP") {
				families[f[2]] = true
			}
			continue
		}
		name, keys, ok := parseSample(line)
		if !ok {
			continue
		}
		samples = append(samples, name)
		for _, k := range keys {
			labels[k] = true
		}
	}
	for _, name := range samples {
		if families[name] {
			continue
		}
		owned := false
		for _, suf := range []string{"_bucket", "_sum", "_count", "_total", "_created"} {
			if b, ok := strings.CutSuffix(name, suf); ok && families[b] {
				owned = true
				break
			}
		}
		if !owned {
			families[name] = true
		}
	}
	delete(labels, "le")
	delete(labels, "quantile")
	return sortedKeys(families), sortedKeys(labels), sc.Err()
}

// parseSample splits `name{k="v",...} value [ts]` into the name and label keys.
func parseSample(line string) (string, []string, bool) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return "", nil, false
	}
	name := line[:i]
	if !validMetricName(name) {
		return "", nil, false
	}
	if line[i] != '{' {
		return name, nil, sampleValue(line[i:])
	}
	var keys []string
	rest := line[i+1:]
	for {
		rest = strings.TrimLeft(rest, " ,")
		if rest == "" {
			return name, keys, false
		}
		if rest[0] == '}' {
			return name, keys, sampleValue(rest[1:])
		}
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			return name, keys, false
		}
		key := strings.TrimSpace(rest[:eq])
		if uq, err := strconv.Unquote(key); err == nil {
			key = uq
		}
		keys = append(keys, key)
		rest = strings.TrimLeft(rest[eq+1:], " ")
		if rest == "" || rest[0] != '"' {
			return name, keys, false
		}
		// Skip the quoted value, honouring escapes.
		j := 1
		for j < len(rest) && rest[j] != '"' {
			if rest[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(rest) {
			return name, keys, false
		}
		rest = rest[j+1:]
	}
}

// sampleValue reports whether rest starts with a sample value, so a line of
// prose is not taken for a sample.
func sampleValue(rest string) bool {
	f := strings.Fields(rest)
	if len(f) == 0 {
		return false
	}
	_, err := strconv.ParseFloat(f[0], 64)
	return err == nil
}

func validMetricName(s string) bool {
	for i, c := range s {
		switch {
		case c == '_' || c == ':' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
