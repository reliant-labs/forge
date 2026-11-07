package cli

// Which dev instance a `forge obs … <env>` reads (control-plane
// docs/design/dev-telemetry-to-hosted.md §1, §6). A local env whose
// `forge env up` sent its telemetry to the hosted store (the recorded
// .forge/obs/backend-<env>.json) shares that env with every other stack of it:
// ten worktrees on one machine, and every teammate's. So a read defaults to
// THIS checkout's instance, the one the record names:
//
//	(default)                 this checkout's instance
//	--instance <id|label>     that instance (a teammate's, or an earlier run)
//	--all-instances           the whole env
//	--user <user>             every instance of one user
//
// The narrowing is the backend's (obsbackend.Instances), so every read a verb
// makes carries it, and the document's scope.instance says which it was.

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/devtelemetry"
	"github.com/reliant-labs/forge/internal/obsbackend"
	"github.com/reliant-labs/forge/internal/obsout"
	"github.com/reliant-labs/forge/pkg/release"
)

// obsInstanceFlags are --instance, --all-instances and --user.
type obsInstanceFlags struct {
	instance string
	all      bool
	user     string
	// userFlag names --user; a verb whose own --user means something else
	// (sessions: the app's user id) registers it as --instance-user.
	userFlag string

	// carry is what a suggested `forge obs` command repeats so it reads what
	// this one read: set by resolve when the narrowing is not the default.
	carry []string
}

func (f *obsInstanceFlags) register(fs *pflag.FlagSet) {
	if f.userFlag == "" {
		f.userFlag = "user"
	}
	fs.StringVar(&f.instance, "instance", "", "read this dev `instance` of a hosted-dev env: its id (sess-…) or its label (alice/fix-login, or the branch alone), resolved through the control plane's Live view (default: this checkout's, as forge env up recorded it)")
	fs.BoolVar(&f.all, "all-instances", false, "read every dev instance of the env, not only this checkout's")
	fs.StringVar(&f.user, f.userFlag, "", "read every dev instance of one `user` (the reliant.instance.user the gateway stamps; the control plane resolves an email)")
}

// sessionIDRe is the shape of sessionIDFor's ids.
var sessionIDRe = regexp.MustCompile(`^sess-[0-9a-f]{24}$`)

// instanceLister reads an env's live `forge env up` sessions, for resolving a
// label; nil when there is no control plane to ask.
type instanceLister func(ctx context.Context, env, environmentID string) ([]release.LocalSession, error)

// resolve turns the flags into the narrowing a backend applies. rec is the
// hosted-dev record the env resolved through (nil otherwise); list resolves a
// label that is not the record's own.
func (f *obsInstanceFlags) resolve(ctx context.Context, env string, rec *devtelemetry.Record, list instanceLister) (obsbackend.Instances, error) {
	f.carry = nil
	if f.all && (f.instance != "" || f.user != "") {
		return obsbackend.Instances{}, obsbackend.Wrong("--all-instances reads every instance: drop --instance and --%s, or drop it", f.flagUser())
	}
	if f.all {
		if rec != nil && rec.InstanceID != "" {
			f.carry = []string{"--all-instances"}
		}
		return obsbackend.Instances{}, nil
	}
	var out obsbackend.Instances
	if f.user != "" {
		out.Users, out.Owner = []string{f.user}, f.user
		f.carry = append(f.carry, "--"+f.flagUser(), f.user)
	}
	switch v := strings.TrimSpace(f.instance); {
	case v == "":
		// The default: this checkout's instance, unless --user widened it.
		if f.user == "" && rec != nil && rec.InstanceID != "" {
			out = recordInstance(rec)
		}
	case rec != nil && (v == rec.InstanceID || v == rec.InstanceLabel):
		in := recordInstance(rec)
		out.IDs, out.Label = in.IDs, in.Label
		if out.Owner == "" {
			out.Owner = in.Owner
		}
		if f.user != "" {
			f.carry = append(f.carry, "--instance", rec.InstanceID)
		}
	case sessionIDRe.MatchString(v):
		out.IDs = []string{v}
		f.carry = append(f.carry, "--instance", v)
	default:
		s, err := instanceByLabel(ctx, env, v, rec, list)
		if err != nil {
			return obsbackend.Instances{}, err
		}
		out.IDs, out.Label = []string{s.ID}, v
		f.carry = append(f.carry, "--instance", s.ID)
	}
	return out, nil
}

func (f *obsInstanceFlags) flagUser() string {
	if f.userFlag == "" {
		return "user"
	}
	return f.userFlag
}

func recordInstance(rec *devtelemetry.Record) obsbackend.Instances {
	return obsbackend.Instances{IDs: []string{rec.InstanceID}, Label: rec.InstanceLabel, Owner: rec.InstanceUser}
}

// instanceByLabel finds the one live session of env whose worktree label is
// label, or the part of it after the last "/" (an instance label is
// "<user>/<worktree label>"; the Live view knows only the worktree's).
func instanceByLabel(ctx context.Context, env, label string, rec *devtelemetry.Record, list instanceLister) (release.LocalSession, error) {
	if list == nil {
		return release.LocalSession{}, obsbackend.Wrong("--instance %q: a label is resolved through the control plane, which env %s is not read through; pass the instance's id (sess-…)", label, env)
	}
	envID := ""
	if rec != nil {
		envID = rec.EnvironmentID
	}
	sessions, err := list(ctx, env, envID)
	if err != nil {
		return release.LocalSession{}, obsbackend.Undetermined("--instance %q: cannot list env %s's instances to resolve the label; pass the instance's id (sess-…)", label, env).Cause(err)
	}
	branch := label
	if i := strings.LastIndex(label, "/"); i >= 0 {
		branch = label[i+1:]
	}
	var hits []release.LocalSession
	var known []string
	for _, s := range sessions {
		if s.Worktree.Label == label || s.Worktree.Label == branch {
			hits = append(hits, s)
		}
		if s.Worktree.Label != "" {
			known = append(known, s.Worktree.Label)
		}
	}
	slices.Sort(known)
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		hint := "it has none live"
		if len(known) > 0 {
			hint = "its live ones are " + strings.Join(slices.Compact(known), ", ")
		}
		return release.LocalSession{}, obsbackend.Wrong("--instance %q names no live instance of env %s (%s); an instance that has stopped is read by its id (sess-…)", label, env, hint)
	}
	ids := make([]string, len(hits))
	for i, s := range hits {
		ids[i] = s.ID
	}
	return release.LocalSession{}, obsbackend.Wrong("--instance %q names %d instances of env %s; pick one by id: %s", label, len(hits), env, strings.Join(ids, ", "))
}

// liveInstanceLister lists env's sessions from the Live view of the control
// plane caller talks to.
func liveInstanceLister(caller obsbackend.Caller, now time.Time) instanceLister {
	return func(ctx context.Context, env, environmentID string) ([]release.LocalSession, error) {
		envs, err := hostedLiveClient{client: caller}.GetLiveView(ctx, hostedProjectName(), false, now)
		if err != nil {
			return nil, err
		}
		for _, e := range envs {
			if (environmentID != "" && e.EnvironmentID == environmentID) || (environmentID == "" && e.Env.Name == env) {
				return e.Sessions, nil
			}
		}
		return nil, nil
	}
}

// carrySuggested repeats a non-default instance narrowing on every suggested
// `forge obs` read, so following a suggestion reads the same instance.
func (f *obsInstanceFlags) carrySuggested(cmds []string) []string {
	if len(f.carry) == 0 {
		return cmds
	}
	tail := " " + obsout.Command(f.carry...)
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = c
		if instanceCarries(c) {
			out[i] = c + tail
		}
	}
	return out
}

// instanceCarries reports whether a suggested command is a telemetry read
// that takes the instance flags.
func instanceCarries(cmd string) bool {
	rest, ok := strings.CutPrefix(cmd, "forge obs ")
	if !ok || strings.Contains(cmd, " --help") || strings.Contains(cmd, "--instance") || strings.Contains(cmd, "--all-instances") {
		return false
	}
	verb, _, _ := strings.Cut(rest, " ")
	switch verb {
	case "changes", "check", "dashboards", "alerts", "profile":
		return false
	}
	return true
}

// obsScopeInstance is the document's scope.instance for i; nil for a read of
// the whole env.
func obsScopeInstance(i obsbackend.Instances) *obsout.ScopeInstance {
	if !i.Narrows() {
		return nil
	}
	s := &obsout.ScopeInstance{Label: i.Label, User: i.Owner}
	if len(i.IDs) > 0 {
		s.ID = i.IDs[0]
	}
	return s
}

// openObsHostedDev builds the cloud backend for a hosted-dev env from its
// record: the env's control plane and the caller's credential, bound to the
// org and environment the record names. No hosted-env lookup runs: a local
// env is not a deploy target, and the record already says which it is.
func openObsHostedDev(ctx context.Context, env string, rec devtelemetry.Record) (obsbackend.Cloud, error) {
	t, err := resolveObsTarget(ctx, env)
	if err != nil {
		return obsbackend.Cloud{}, obsbackend.Undetermined("env %s: cannot read its declaration", env).Cause(err).
			With(obsbackend.Cmd("forge", "env", "list"))
	}
	ep, cred, err := resolveCloudTargetFor(env, t.Declaration(), "")
	if err != nil {
		return obsbackend.Cloud{}, obsbackend.Undetermined("env %s: cannot reach its control plane", env).Cause(err)
	}
	return obsbackend.Cloud{Env: env, EnvID: rec.EnvironmentID, OrgID: rec.OrgID, Caller: cloud.NewClient(ep, cred)}, nil
}

// readDevTelemetry reads env's recorded telemetry decision. A record that
// cannot be read is reported and ignored: the env then resolves as if `forge
// env up` had recorded nothing.
func readDevTelemetry(projectDir, env string, warn func(string)) *devtelemetry.Record {
	rec, ok, err := devtelemetry.Read(projectDir, env)
	if err != nil {
		warn(fmt.Sprintf("ignoring %s: %v", devtelemetry.Path(projectDir, env), err))
		return nil
	}
	if !ok {
		return nil
	}
	return &rec
}

// hostedDevBackend is a hosted-dev env's backend: its telemetry is read from
// the control plane, its changes from this machine's ledger, as for any local
// env (a local env has no promotions on the control plane).
type hostedDevBackend struct {
	obsbackend.Cloud
	ledger obsbackend.ChangesReader
}

// Changes implements obsbackend.ChangesReader.
func (h hostedDevBackend) Changes(ctx context.Context, req obsbackend.ChangesRequest) (obsbackend.ChangesResult, error) {
	if h.ledger == nil {
		return obsbackend.ChangesResult{}, obsbackend.Undetermined("no ledger of env %s's changes on this machine", h.Env)
	}
	return h.ledger.Changes(ctx, req)
}

// instanceName is how a text header names an instance: its label and id, or
// whichever it has, and its user.
func instanceName(in obsout.ScopeInstance) string {
	name := in.ID
	switch {
	case in.Label != "" && in.ID != "":
		name = in.Label + " (" + in.ID + ")"
	case in.Label != "":
		name = in.Label
	case in.ID == "":
		name = "every instance"
	}
	if in.User != "" && !strings.HasPrefix(in.Label, in.User) {
		name += " of " + in.User
	}
	return name
}
