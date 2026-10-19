package serverkit_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/serverkit"
)

// drain_phases_test.go — a shutdown says where its time went.
//
// The sequence used to log "server stopping" and then nothing until an error,
// so a rollout that took the whole grace period could not be diagnosed from
// its own log: the pre-stop pause, a stream nobody ended, a worker ignoring
// its context and a teardown blocked on a pool all looked the same.

// TestRun_LogsEachDrainPhase: every phase writes one "drain: ..." line, in
// order, each carrying `phase` and `elapsed`, and the listener's says what the
// drain did to the streams that were open.
func TestRun_LogsEachDrainPhase(t *testing.T) {
	requireSelfSIGTERM(t)
	// Not parallel — sends SIGTERM.
	addr := freeAddr(t)
	var logs syncBuf
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mux := http.NewServeMux()
	mux.Handle("/events", serverkit.LongLived(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush() //nolint:errcheck // test stream
		<-r.Context().Done()
	})))
	srv := serverkit.Server{Handler: mux, Logger: logger}
	srv.AddWorker(&stubWorker{name: "sweeper"})
	errCh, _ := runInBackground(t, baseConfig(addr), srv)
	waitReady(t, addr, 5*time.Second)

	stream := openStream(t, "http://"+addr+"/events")
	defer func() { _ = stream.Close() }()
	go func() { _, _ = io.Copy(io.Discard, stream) }()

	if err := shutdownAndWait(t, errCh, 30*time.Second); err != nil {
		t.Fatalf("Run returned %v", err)
	}

	var phases []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && strings.HasPrefix(rec["msg"].(string), "drain: ") {
			phases = append(phases, rec)
		}
	}
	var order []string
	for _, rec := range phases {
		order = append(order, rec["phase"].(string))
		if _, ok := rec["elapsed"]; !ok && rec["phase"] != "readiness" {
			t.Errorf("%q has no elapsed: %v", rec["msg"], rec)
		}
	}
	want := []string{"readiness", "pre_stop", "listener", "workers", "teardown", "total"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("drain phases logged = %v, want %v\n%s", order, want, logs.String())
	}
	if open := phases[0]["streams_open"]; open != float64(1) {
		t.Errorf("readiness line streams_open = %v, want 1", open)
	}
	listener := phases[2]
	if listener["streams_ended"] != float64(1) || listener["streams_still_running"] != float64(0) {
		t.Errorf("listener line = %v, want streams_ended=1 streams_still_running=0", listener)
	}
	if phases[3]["workers"] != float64(1) {
		t.Errorf("workers line = %v, want workers=1", phases[3])
	}
}

// The drain's own accounting, which the listener line reports.
func TestStreams_ReportCountsWhatTheDrainDid(t *testing.T) {
	t.Parallel()
	streams := &serverkit.Streams{DrainReserve: 400 * time.Millisecond}
	quick, releaseQuick := streams.Bind(context.Background())
	go func() { <-quick.Done(); releaseQuick() }()
	held, releaseHeld := streams.Bind(context.Background())
	_ = serverkit.Draining(held) // claims the soft phase, then ignores it
	go func() { <-held.Done(); releaseHeld() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := streams.Drain(ctx); err != nil {
		t.Fatalf("Drain = %v", err)
	}
	streams.BeginDrain() // a second call — NewHTTPServer's hook — counts nothing twice
	_, late := streams.Bind(context.Background())
	late()

	got := streams.Report()
	want := serverkit.DrainReport{Ended: 2, Claimed: 1, HardEnded: 1}
	if got != want {
		t.Fatalf("Report = %+v, want %+v (one ended at once, one claimed then hard-ended, one refused late)", got, want)
	}
}
