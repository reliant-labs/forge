// File: profiler.go — continuous profiling, pushed as OTLP profiles.
//
// Setup starts one background goroutine per process (when an OTLP endpoint is
// set and OTEL_PROFILES_EXPORTER is not "none"). About once a minute it
// captures a 10s CPU profile plus a heap and a goroutine snapshot, converts them
// with profiles_convert.go, and POSTs them to the collector's profiles endpoint.
// Measured cost: ~90 ms CPU and 1.5 MB allocated per cycle to convert, i.e.
// ~0.15% of one core, plus the runtime's own ~100 Hz CPU sampling during the
// 10s window.
//
// The loop is built so it can never hurt the application:
//
//   - it runs on its own goroutine and nothing in the request path waits on it;
//   - a panic inside a cycle is recovered and counted as a failed cycle;
//   - every cycle starts with a cheap TCP reachability probe, so an unreachable
//     collector costs one dial, not a 10s CPU profile;
//   - consecutive failures back off exponentially (to 15 minutes) and the first
//     is logged once at WARN, the rest at DEBUG;
//   - if another CPU profile is already running (an operator on /debug/pprof),
//     this cycle skips the CPU profile instead of fighting for it;
//   - the first capture and every interval after it are jittered, so a
//     deployment's replicas do not all profile on the same second.

package observe

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"runtime/pprof"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/collector/pdata/pprofile"
	"go.opentelemetry.io/collector/pdata/pprofile/pprofileotlp"
	"go.opentelemetry.io/otel/sdk/resource"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	profilerInterval   = 60 * time.Second
	profilerCPUWindow  = 10 * time.Second
	profilerMaxBackoff = 15 * time.Minute
	profilerProbeWait  = 2 * time.Second
	profilerJitter     = 0.2
)

// profilingActive is true while a profiler goroutine runs. The request-side
// label helpers read it so a process that does not profile pays nothing per
// request.
var profilingActive atomic.Bool

type profiler struct {
	res *resource.Resource

	interval     time.Duration
	window       time.Duration
	heapInterval time.Duration
	lastHeap     time.Time // last attempt, not success: errors must not amplify captures

	probe func(context.Context) error
	send  func(context.Context, pprofile.Profiles) error
	log   *slog.Logger

	// sleep waits d or until ctx ends. A field so tests do not wait.
	sleep func(context.Context, time.Duration) bool

	failures int
}

// startProfiler resolves the destination and starts the capture loop. The
// returned function stops it. An error means profiling is off, never that the
// service should not start.
func startProfiler(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error) {
	tgt, err := resolveProfilesTarget(cfg)
	if err != nil {
		return nil, err
	}
	send, closeSender, err := newProfilesSender(tgt)
	if err != nil {
		return nil, err
	}
	p := &profiler{
		res:          res,
		interval:     profilerInterval,
		heapInterval: cfg.ProfilesHeapInterval,
		window:       profilerCPUWindow,
		probe:        tcpProbe(tgt),
		send:         send,
		log:          slog.Default().With("component", "profiler"),
		sleep:        sleepCtx,
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	profilingActive.Store(true)
	go func() {
		defer close(done)
		p.run(loopCtx)
	}()
	return func(shutdownCtx context.Context) error {
		cancel()
		profilingActive.Store(false)
		select {
		case <-done:
		case <-shutdownCtx.Done():
		}
		return closeSender()
	}, nil
}

// run is the capture loop. It returns when ctx ends.
func (p *profiler) run(ctx context.Context) {
	// First capture lands somewhere in [interval/4, interval): soon enough to
	// see the process, spread enough that replicas do not align.
	if !p.sleep(ctx, p.interval/4+rand.N(p.interval*3/4)) {
		return
	}
	for {
		err := p.safeCycle(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.failures++
			if p.failures == 1 {
				p.log.Warn("continuous profiling cannot reach the collector; backing off", "error", err)
			} else {
				p.log.Debug("continuous profiling cycle failed", "error", err, "consecutive", p.failures)
			}
		} else {
			if p.failures > 0 {
				p.log.Info("continuous profiling recovered", "after_failures", p.failures)
			}
			p.failures = 0
		}
		if !p.sleep(ctx, p.nextWait()) {
			return
		}
	}
}

// nextWait is the pause before the next cycle: the interval, doubled per
// consecutive failure up to profilerMaxBackoff, jittered by ±profilerJitter.
func (p *profiler) nextWait() time.Duration {
	d := p.interval
	for i := 0; i < p.failures && d < profilerMaxBackoff; i++ {
		d *= 2
	}
	if d > profilerMaxBackoff {
		d = profilerMaxBackoff
	}
	j := 1 + profilerJitter*(2*rand.Float64()-1)
	return time.Duration(float64(d) * j)
}

// safeCycle runs one cycle and turns a panic into an error.
func (p *profiler) safeCycle(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("profiler panicked: %v", r)
		}
	}()
	return p.cycle(ctx)
}

func (p *profiler) cycle(ctx context.Context) error {
	pctx, cancel := context.WithTimeout(ctx, profilerProbeWait)
	err := p.probe(pctx)
	cancel()
	if err != nil {
		return err
	}

	var cpu bytes.Buffer
	cpuOn := pprof.StartCPUProfile(&cpu) == nil
	if !cpuOn {
		p.log.Debug("another CPU profile is running; capturing heap and goroutines only")
	} else {
		p.sleep(ctx, p.window)
		pprof.StopCPUProfile()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	var heap, gor bytes.Buffer
	if p.heapDue(time.Now()) {
		if err := pprof.Lookup("heap").WriteTo(&heap, 0); err != nil {
			return fmt.Errorf("heap profile: %w", err)
		}
	}
	if err := pprof.Lookup("goroutine").WriteTo(&gor, 0); err != nil {
		return fmt.Errorf("goroutine profile: %w", err)
	}

	var errs []error
	for _, raw := range []*bytes.Buffer{&cpu, &heap, &gor} {
		if raw.Len() == 0 {
			continue
		}
		pd, err := pprofToOTLP(raw, p.res)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if pd.SampleCount() == 0 {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = p.send(sctx, pd)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// heapDue claims a capture slot even if conversion/export subsequently fails.
// This bounds full-snapshot attempts, not their size. No delta baseline, retry
// backlog, or catch-up burst: each later capture is independently interpretable.
func (p *profiler) heapDue(now time.Time) bool {
	if p.heapInterval <= 0 {
		return true // preserve default cadence; policy belongs to the owner
	}
	if !p.lastHeap.IsZero() && now.Sub(p.lastHeap) < p.heapInterval {
		return false
	}
	p.lastHeap = now
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// tcpProbe returns a function that dials the profiles destination, the
// cheapest thing that tells "nobody is listening" from "worth profiling for".
func tcpProbe(t profilesTarget) func(context.Context) error {
	addr := t.URL
	if t.Protocol != ProtocolGRPC {
		if u, err := url.Parse(t.URL); err == nil {
			addr = u.Host
			if u.Port() == "" {
				port := "80"
				if u.Scheme == "https" {
					port = "443"
				}
				addr = net.JoinHostPort(u.Hostname(), port)
			}
		}
	}
	return func(ctx context.Context) error {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
}

// newProfilesSender builds the transport for t. The second result releases it.
func newProfilesSender(t profilesTarget) (func(context.Context, pprofile.Profiles) error, func() error, error) {
	if t.Protocol == ProtocolGRPC {
		creds := credentials.NewTLS(nil)
		if t.Insecure {
			creds = insecure.NewCredentials()
		}
		conn, err := grpc.NewClient(t.URL, grpc.WithTransportCredentials(creds))
		if err != nil {
			return nil, nil, fmt.Errorf("dial OTLP profiles endpoint: %w", err)
		}
		client := pprofileotlp.NewGRPCClient(conn)
		send := func(ctx context.Context, pd pprofile.Profiles) error {
			md := metadata.New(t.Headers)
			if t.FileHeaders != nil {
				t.FileHeaders.each(func(k, v string) { md.Set(k, v) })
			}
			if md.Len() > 0 {
				ctx = metadata.NewOutgoingContext(ctx, md)
			}
			_, err := client.Export(ctx, pprofileotlp.NewExportRequestFromProfiles(pd))
			return err
		}
		return send, conn.Close, nil
	}

	hc := &http.Client{Timeout: t.Timeout}
	send := func(ctx context.Context, pd pprofile.Profiles) error {
		body, err := pprofileotlp.NewExportRequestFromProfiles(pd).MarshalProto()
		if err != nil {
			return fmt.Errorf("marshal profiles: %w", err)
		}
		hdr := http.Header{"Content-Type": {"application/x-protobuf"}}
		if t.Gzip {
			var zb bytes.Buffer
			zw := gzip.NewWriter(&zb)
			_, _ = zw.Write(body)
			_ = zw.Close()
			body = zb.Bytes()
			hdr.Set("Content-Encoding", "gzip")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header = hdr
		if t.FileHeaders != nil {
			t.FileHeaders.each(func(k, v string) { req.Header.Set(k, v) })
		}
		for k, v := range t.Headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			msg := fmt.Sprintf("OTLP profiles endpoint answered %s", resp.Status)
			if resp.StatusCode == http.StatusNotFound {
				msg += " (the collector must run with --feature-gates=+service.profilesSupport)"
			}
			return errors.New(msg)
		}
		return nil
	}
	return send, func() error { return nil }, nil
}
