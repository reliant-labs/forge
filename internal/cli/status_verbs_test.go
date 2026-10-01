package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

// Tests for F7's status verbs (hosted-deploy-primitives §3.5): env history,
// release where, and env topology's additive fields. The hosted cases run the
// real hostedStore over cloud.Client against fakeDeployService, whose
// ListPromotions applies C7's keyset cursor and version filter.

// pageAll walks a history to the end, the way a reader is told to: stop on an
// empty cursor, not on a short page.
func pageAll(t *testing.T, r bindingHistoryReader, env string, q historyQuery) [][]string {
	t.Helper()
	var pages [][]string
	for guard := 0; guard < 20; guard++ {
		page, err := r.HistoryPage(context.Background(), env, q)
		if err != nil {
			t.Fatalf("history page: %v", err)
		}
		var rels []string
		for _, p := range page.Promotions {
			rels = append(rels, p.Release)
		}
		pages = append(pages, rels)
		if page.Next == "" {
			return pages
		}
		q.Before = page.Next
	}
	t.Fatal("history did not terminate")
	return nil
}

// Both backends page the SAME way: newest first, keyset cursor, empty cursor
// on the last page. A reader written against one must work on the other.
func TestHistoryPage_BothBackendsPageIdentically(t *testing.T) {
	// File: prod v1 → v2 → v3 → v1 → v4.
	dir := t.TempDir()
	file := newFileBindingStore(dir)
	for _, v := range []string{"v1", "v2", "v3", "v1", "v4"} {
		writeBinding(t, dir, "prod", v, map[string]string{"api": sha("a")})
	}
	// Hosted: the same sequence through the Promote RPC.
	_, hosted := hostedPromoteFixture(t, "v1", "v2", "v3")
	for _, v := range []string{"v1", "v4"} {
		if v == "v4" {
			r := ociRelease("v4", map[string]string{"api": sha("4")})
			if _, err := hosted.Cut(context.Background(), r); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := hosted.Append(context.Background(), release.Promotion{Env: "prod", Release: v, Kind: release.KindPromote}, appendGuard{}); err != nil {
			t.Fatal(err)
		}
	}

	want := [][]string{{"v4", "v1"}, {"v3", "v2"}, {"v1"}}
	for name, r := range map[string]bindingHistoryReader{"file": file, "hosted": hosted} {
		got := pageAll(t, r, "prod", historyQuery{Limit: 2})
		if !equalPages(got, want) {
			t.Errorf("%s pages = %v, want %v", name, got, want)
		}
		// The version filter keeps only v1's two promotions, newest first.
		page, err := r.HistoryPage(context.Background(), "prod", historyQuery{Release: "v1"})
		if err != nil || len(page.Promotions) != 2 || page.Next != "" {
			t.Errorf("%s --release v1 = %+v %v, want 2 entries and no next", name, page, err)
		}
	}
}

func equalPages(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Join(a[i], ",") != strings.Join(b[i], ",") {
			return false
		}
	}
	return true
}

// A cursor that names no promotion of this env: the file ledger refuses it
// (exit 1 — the request, not the ledger), so a pipeline paging with a typo
// does not loop over page one forever.
func TestEnvHistory_BadRequestIsExit1(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "prod", "v1", map[string]string{"api": sha("a")})
	store := newFileBindingStore(dir)
	for name, q := range map[string]historyQuery{
		"unknown cursor": {Before: "no-such-id"},
		"limit too big":  {Limit: maxHistoryLimit + 1},
	} {
		var buf bytes.Buffer
		err := runEnvHistory(context.Background(), store, "prod", q, false, &buf)
		if got := exitCodeForError(err); got != exitWrong {
			t.Errorf("%s: exit %d, want %d (%v)", name, got, exitWrong, err)
		}
	}
}

// --json carries the ledger entries with both evidence halves counted, the
// cursor, and the envelope. The capture recipe the CI template uses works.
func TestEnvHistory_JSON(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	writeBinding(t, dir, "prod", "v1", map[string]string{"api": sha("a")})
	if _, err := store.Append(context.Background(), release.Promotion{
		Env: "prod", Release: "v2", Kind: release.KindPromote, FromEnv: "staging", Note: "hotfix",
		Resolved: map[string]string{"api": sha("b")},
		Gates: []release.Gate{
			{Name: "lint", Status: release.GateStatusPassed},
			{Name: "test", Status: release.GateStatusFailed},
		},
	}, appendGuard{}); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		err = runEnvHistory(context.Background(), store, "prod", historyQuery{Limit: 1}, true, &bytes.Buffer{})
	})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	var doc struct {
		OK         bool `json:"ok"`
		Promotions []struct {
			ID      string        `json:"id"`
			Release string        `json:"release"`
			FromEnv string        `json:"from_env"`
			Note    string        `json:"note"`
			Gates   *gatesSummary `json:"gates_summary"`
		} `json:"promotions"`
		NextBefore string `json:"next_before"`
	}
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("decode: %v\n%s", jerr, out)
	}
	if !doc.OK || len(doc.Promotions) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	p := doc.Promotions[0]
	if p.Release != "v2" || p.FromEnv != "staging" || p.Note != "hotfix" || p.ID == "" {
		t.Errorf("newest entry = %+v", p)
	}
	if p.Gates == nil || p.Gates.Passed != 1 || p.Gates.Failed != 1 {
		t.Errorf("gates_summary = %+v, want 1 passed 1 failed", p.Gates)
	}
	if doc.NextBefore != p.ID {
		t.Errorf("next_before = %q, want the last served id %q (one more entry remains)", doc.NextBefore, p.ID)
	}
}

// release where: hosted answers from GetRelease.current_environment_ids,
// RESOLVED TO NAMES; a release cut but bound nowhere is exit 1; one never cut
// is exit 1 with a different reason.
func TestReleaseWhere_Hosted(t *testing.T) {
	fake, store := hostedPromoteFixture(t, "v1", "v2")
	run := func(version string) (string, error) {
		var err error
		out := captureStdout(t, func() {
			err = runReleaseWhere(context.Background(), version, []whereSource{{Location: "cp", Hosted: store.client}}, true, &bytes.Buffer{})
		})
		return out, err
	}
	out, err := run("v2")
	if err != nil {
		t.Fatalf("v2 is current on prod: %v", err)
	}
	var doc releaseWhereDocument
	if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
		t.Fatalf("decode: %v\n%s", jerr, out)
	}
	if len(doc.Environments) != 1 || doc.Environments[0].Env != "prod" {
		t.Fatalf("environments = %+v, want [prod] by NAME", doc.Environments)
	}
	if _, err := run("v1"); exitCodeForError(err) != exitWrong {
		t.Errorf("v1 (cut, superseded) must exit %d, got %v", exitWrong, err)
	}
	if _, err := run("v404"); exitCodeForError(err) != exitWrong {
		t.Errorf("a never-cut release must exit %d, got %v", exitWrong, err)
	}
	_ = fake
}

func TestReleaseWhere_File(t *testing.T) {
	dir := t.TempDir()
	writeBinding(t, dir, "staging", "v2", map[string]string{"api": sha("b")})
	writeBinding(t, dir, "prod", "v1", map[string]string{"api": sha("a")})
	writeBinding(t, dir, "prod", "v2", map[string]string{"api": sha("b")})
	store := newFileBindingStore(dir)
	src := whereSource{Location: dir, Files: map[string]bindingStore{"staging": store, "prod": store, "dev": store}}

	var buf bytes.Buffer
	if err := runReleaseWhere(context.Background(), "v2", []whereSource{src}, false, &buf); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if !strings.Contains(buf.String(), "prod, staging") {
		t.Errorf("want both envs, sorted, got:\n%s", buf.String())
	}
	if err := runReleaseWhere(context.Background(), "v1", []whereSource{src}, false, &bytes.Buffer{}); exitCodeForError(err) != exitWrong {
		t.Errorf("v1 (superseded on prod) must exit %d, got %v", exitWrong, err)
	}
}

// topology --json gains promotion_id, from_env and gates_summary, ADDITIVELY.
func TestEnvTopology_AdditiveF7Fields(t *testing.T) {
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	got, err := store.Append(context.Background(), release.Promotion{
		Env: "prod", Release: "v1", Kind: release.KindPromote, FromEnv: "staging",
		Resolved: map[string]string{"api": sha("a")},
		Gates:    []release.Gate{{Name: "lint", Status: release.GateStatusPassed}},
	}, appendGuard{})
	if err != nil {
		t.Fatal(err)
	}
	ledgers := func(context.Context, string) (envLedger, error) {
		return envLedger{Bindings: store, Releases: fileReleaseLedger{projectDir: dir}}, nil
	}
	row := buildTopologyEnvRow(context.Background(), dir, "prod", false, map[string]release.Release{}, nil,
		envTopologyOptions{Ledgers: ledgers})
	if row.PromotionID != got.ID || row.FromEnv != "staging" {
		t.Errorf("promotion_id/from_env = %q/%q, want %q/staging", row.PromotionID, row.FromEnv, got.ID)
	}
	if row.GatesSummary == nil || row.GatesSummary.Passed != 1 {
		t.Errorf("gates_summary = %+v", row.GatesSummary)
	}
	raw, _ := json.Marshal(row)
	for _, key := range []string{`"promotion_id"`, `"from_env"`, `"gates_summary"`, `"release"`, `"bound"`, `"images"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("topology row JSON lacks %s:\n%s", key, raw)
		}
	}
}

// `env rollout` IS `env wait --timeout 0`: the command sets Once and hands the
// SAME options to the SAME wait (§3.5). Asserted at the seam F3 exposes, so a
// rollout that grew its own read would fail here.
func TestEnvRollout_IsASingleReadWait(t *testing.T) {
	var got []envWaitOptions
	prev := runEnvWaitForCmd
	runEnvWaitForCmd = func(_ context.Context, _ string, opts envWaitOptions) error {
		got = append(got, opts)
		return nil
	}
	t.Cleanup(func() { runEnvWaitForCmd = prev })

	cmd := newEnvRolloutCmd()
	cmd.SetArgs([]string{"prod", "--promotion", "promo-7", "--json"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Once || got[0].PromotionID != "promo-7" || !got[0].JSON {
		t.Fatalf("env rollout must run ONE single-read wait on the named promotion, got %+v", got)
	}
	if got[0].Timeout != 0 || got[0].FailFast {
		t.Errorf("a single read has no budget and nothing to fail fast on: %+v", got[0])
	}
}

// Hosted `env verify` end to end through runEnvVerify: the observer's answer,
// mapped to the five states, with the cluster path's exit codes, and
// `source: control-plane observer` in --json. A stale observation (UNKNOWN
// from the server since control-plane #491) is UNREACHABLE → exit 2, never 0.
func TestEnvVerify_HostedReadsTheObserver(t *testing.T) {
	pin := sha("1")
	_, store := hostedPromoteFixture(t, "v1")
	cur, _, _ := store.Current(context.Background(), "prod")

	cases := []struct {
		name  string
		phase string
		seen  string
		want  int
	}{
		{"observer saw the pin", wireRolloutPhaseSucceeded, pin, exitOK},
		{"observer saw another digest", wireRolloutPhaseProgressing, sha("9"), exitWrong},
		{"stale observation", wireRolloutPhaseUnknown, pin, exitUndetermined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var askedFor string
			read := func(_ context.Context, _ string, promotionID string) (wireRollout, error) {
				askedFor = promotionID
				return wireRollout{Workloads: []wireWorkloadRollout{{
					Name: "api", Artifact: "api", PinnedDigest: pin, ObservedDigest: tc.seen,
					ObservedState: "DEPLOY_OBSERVED_STATE_READY", Phase: tc.phase,
				}}}, nil
			}
			t.Chdir(t.TempDir())
			var err error
			out := captureStdout(t, func() {
				err = runEnvVerify(context.Background(), "prod", envVerifyOptions{
					JSON: true, Bindings: store, HostedRollout: read,
					Resolver: stubResolver{}, // must NOT be consulted for a hosted env
				})
			})
			if got := exitCodeForError(err); got != tc.want {
				t.Fatalf("exit = %d, want %d (%v)\n%s", got, tc.want, err, out)
			}
			if askedFor != cur.ID {
				t.Errorf("verify must read the CURRENT promotion %q's rollout, asked for %q", cur.ID, askedFor)
			}
			var doc envVerifyReport
			if jerr := json.Unmarshal([]byte(out), &doc); jerr != nil {
				t.Fatalf("decode: %v\n%s", jerr, out)
			}
			if doc.Source != hostedVerifySource || doc.KubeContext != "" {
				t.Errorf("source/kube_context = %q/%q, want %q/\"\"", doc.Source, doc.KubeContext, hostedVerifySource)
			}
		})
	}
}

// topology --json carries the hosted current promotion's rollout phase.
func TestEnvTopology_HostedRolloutPhase(t *testing.T) {
	_, store := hostedPromoteFixture(t, "v1")
	ledgers := func(context.Context, string) (envLedger, error) {
		return envLedger{Bindings: store, Releases: store, Hosted: true}, nil
	}
	rollouts := func(context.Context, string, string) (wireRollout, error) {
		return wireRollout{Phase: wireRolloutPhaseStabilizing}, nil
	}
	row := buildTopologyEnvRow(context.Background(), t.TempDir(), "prod", false, map[string]release.Release{}, nil,
		envTopologyOptions{Ledgers: ledgers, Rollouts: rollouts})
	if row.RolloutPhase != "stabilizing" || row.PromotionID == "" {
		t.Fatalf("rollout_phase/promotion_id = %q/%q", row.RolloutPhase, row.PromotionID)
	}
}
