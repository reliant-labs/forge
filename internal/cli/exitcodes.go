package cli

// The exit-code table every hosted deploy verb shares.
//
// It extends the convention `forge release verify` and `forge env verify`
// already follow, where the distinction that matters is not pass/fail but
// WHETHER WE LOOKED: 1 means "looked, and it is wrong", and 2 means "could
// not determine". That difference is what lets CI tell a bad release from a
// broken control plane, and collapsing the two into a generic failure is the
// thing this table exists to prevent.
//
// CI branches on these codes directly, which is why they are named here
// rather than written as literals at each `exitCodeError{code: …}`. A verb
// that invented its own numbering would silently change the meaning of a
// pipeline's `if [ $? -eq 3 ]`, and a reader of that pipeline has no way to
// discover the drift.
//
// 3–6 are the codes the hosted primitives add. Each names ONE outcome a
// pipeline reacts to differently:
//
//   - 3 (conflict) means someone else moved the environment. The pipeline
//     must stop and page a human: retrying would stomp whatever landed.
//   - 4 (refused) means the write was declined, not lost — a rollout is in
//     flight, or the environment is pinned. A pipeline may wait and retry.
//   - 5 (timed out) means the rollout was still progressing when the budget
//     ran out. Retrying the WAIT is correct; re-promoting is not.
//   - 6 (superseded) means a newer promotion replaced the one being waited
//     on. The wait's subject is gone, so neither retry nor failure is right
//     — the pipeline's release was overtaken.
//
// 5 and 6 are deliberately NOT 1. A timeout and an overtaken wait are both
// "we never saw this finish", and reporting either as "the release is bad"
// would fail builds for releases that were fine.
const (
	// exitOK is success, or an idempotent no-op. A re-run of a promote
	// that already landed exits 0: reaching the requested end state is
	// what was asked for.
	exitOK = 0

	// exitWrong: we looked, and it is wrong — degraded, drifted, a check
	// failed, or the input was invalid. Raised by wait, verify and record.
	exitWrong = 1

	// exitUndetermined: we could not look. The control plane was
	// unreachable, auth was refused, or the thing is unobservable. Never
	// folded into success or failure: "we cannot see it" is not
	// permission.
	exitUndetermined = 2

	// exitConflict: the environment's current promotion is not the one the
	// caller expected (a compare-and-set mismatch). Promote only.
	exitConflict = 3

	// exitRefused: the write was declined — a rollout is in flight and
	// --supersede was not given, or the environment is pinned. Promote
	// only.
	exitRefused = 4

	// exitTimedOut: the budget expired while the rollout was still
	// progressing. Wait only.
	exitTimedOut = 5

	// exitSuperseded: the environment was promoted past the promotion
	// being waited on. Wait only.
	exitSuperseded = 6
)

// The domain reasons a control plane sends on a refused promote, under
// cloud.ReasonHeader. The Connect code for all three is FailedPrecondition —
// the CATEGORY — and the reason is what says which refusal happened.
const (
	// reasonPromotionConflict: the environment's current promotion is not
	// the one the caller's compare-and-set expected.
	reasonPromotionConflict = "promotion_conflict"
	// reasonRolloutInFlight: the current promotion has not finished
	// rolling out, and --supersede was not given.
	reasonRolloutInFlight = "rollout_in_flight"
	// reasonEnvironmentPinned: the environment's reconcile policy is
	// pinned, which refuses every change.
	reasonEnvironmentPinned = "environment_pinned"
	// reasonSourceMoved: `promote --from` named a source promotion that is
	// no longer the source environment's current one. A conflict — someone
	// else moved the source — so it shares exitConflict.
	reasonSourceMoved = "source_moved"
)

// exitCodeForRefusal maps a refusal REASON to its exit code — the one table
// every verb reads, so a pipeline's `if [ $? -eq 3 ]` means the same thing
// whichever verb produced it.
//
// Conflict and refused are separated because a pipeline reacts to them
// oppositely. A conflict (3) means someone else moved the environment, so
// retrying would stomp whatever landed and the run must stop. A refusal (4)
// means the write was declined but nothing was lost — a rollout is in
// flight, or the env is pinned — so waiting and retrying is legitimate.
// Collapsing both into one code would force every pipeline to re-derive the
// difference from message text, which is the thing §3.A exists to stop.
//
// An unrecognised reason is exitWrong, NOT exitUndetermined: the server
// refused, which means it looked. A reason forge does not know about is a
// forge gap, not an unobservable environment, and reporting it as 2 would
// tell CI to retry a write that will be refused identically.
func exitCodeForRefusal(reason string) int {
	switch reason {
	case reasonPromotionConflict, reasonSourceMoved:
		return exitConflict
	case reasonRolloutInFlight, reasonEnvironmentPinned:
		return exitRefused
	default:
		return exitWrong
	}
}
