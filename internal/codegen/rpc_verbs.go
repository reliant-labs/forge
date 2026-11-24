// File: internal/codegen/rpc_verbs.go
//
// What an rpc's NAME says about whether it reads or writes, for the passes
// that ask the proto to state it: `forge lint --idempotency` (does the
// idempotency_level agree with the name?) and `forge scaffold rpc` (which
// prints the declaration in the snippet it hands the author).
//
// The first word of the name decides, matched as a whole word (CheckIn
// starts with "Check"; Checkout does not), after one leading Batch or Bulk
// (BatchGetOrders reads, BulkDeleteOrders writes).

package codegen

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/reliant-labs/forge/pkg/activity"
)

// rpcWriteVerbs are the rpc-name verbs whose every reading changes state.
// Sorted. Verbs a pure read may plausibly use stay out — Validate, Preview,
// Export, Render, Resolve, Sync, Refresh, Generate, Verify — because a read
// verb on this list would make a correct NO_SIDE_EFFECTS a lint finding,
// while a write verb missing from it only leaves one case to review.
var rpcWriteVerbs = []string{
	"Abort", "Accept", "Ack", "Acknowledge", "Activate", "Add", "Allocate", "Append",
	"Apply", "Approve", "Archive", "Assign", "Attach", "Ban", "Block", "Bump",
	"Cancel", "Charge", "Claim", "Clear", "Close", "Commit", "Complete", "Confirm",
	"Create", "Deactivate", "Decline", "Decrement", "Delete", "Deploy", "Deprovision",
	"Destroy", "Detach", "Disable", "Disconnect", "Dismiss", "Drop", "Edit", "Enable",
	"Enqueue", "Ensure", "Evict", "Execute", "Expire", "Finish", "Fork", "Grant",
	"Import", "Increment", "Insert", "Install", "Invite", "Invoke", "Join", "Kill",
	"Leave", "Link", "Lock", "Logout", "Mark", "Merge", "Move", "Mute", "Patch",
	"Pause", "Pin", "Post", "Promote", "Provision", "Publish", "Purge", "Push", "Put",
	"Reassign", "Record", "Redeem", "Refund", "Register", "Reject", "Release",
	"Remove", "Rename", "Reorder", "Replace", "Reply", "Reserve", "Reset", "Restart",
	"Restore", "Resume", "Retry", "Revoke", "Rollback", "Rotate", "Run", "Save",
	"Schedule", "Send", "Set", "Share", "Spawn", "Start", "Stop", "Submit",
	"Subscribe", "Suspend", "Terminate", "Toggle", "Transfer", "Trigger", "Unarchive",
	"Unassign", "Unban", "Unblock", "Uninstall", "Unlink", "Unlock", "Unmute", "Unpin",
	"Unregister", "Unset", "Unshare", "Unsubscribe", "Update", "Upload", "Upsert",
	"Write",
}

// WriteVerbs returns the write verbs WriteVerb recognizes, sorted.
func WriteVerbs() []string { return append([]string(nil), rpcWriteVerbs...) }

// rpcBatchPrefixes may open an rpc name ahead of its verb.
var rpcBatchPrefixes = []string{"Batch", "Bulk"}

// ReadVerb is the read verb an rpc name opens with, or "" when it opens with
// none forge treats as a read.
//
// The read verbs are exactly the names forge ALREADY treats as reads: the
// hook generator's (IsQueryMethodName: Get, List, Search, Find, Check, Has,
// Is, Count, Exists) and the activity interceptor's
// (activity.IsMutatingName: BatchGet, Get, List, Watch, Search, Query,
// Count, Describe). So asking every rpc ReadVerb names to declare
// NO_SIDE_EFFECTS can never ask it of a name forge itself would classify as
// a write.
func ReadVerb(name string) string {
	for _, n := range []string{cutBatch(name), name} {
		if !IsQueryMethodName(n) && activity.IsMutatingName(n) {
			continue
		}
		return leadingWord(n)
	}
	return ""
}

// WriteVerb is the write verb (rpcWriteVerbs) an rpc name opens with, or "".
func WriteVerb(name string) string {
	n := cutBatch(name)
	for _, v := range rpcWriteVerbs {
		if rest, ok := strings.CutPrefix(n, v); ok && (rest == "" || startsWord(rest)) {
			return v
		}
	}
	return ""
}

// cutBatch strips one leading Batch or Bulk word.
func cutBatch(name string) string {
	for _, p := range rpcBatchPrefixes {
		if rest, ok := strings.CutPrefix(name, p); ok && startsWord(rest) {
			return rest
		}
	}
	return name
}

// startsWord reports whether s begins a new PascalCase word.
func startsWord(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return s != "" && (unicode.IsUpper(r) || unicode.IsDigit(r))
}

// leadingWord is the first PascalCase word of name.
func leadingWord(name string) string {
	for i, r := range name {
		if i > 0 && (unicode.IsUpper(r) || unicode.IsDigit(r)) {
			return name[:i]
		}
	}
	return name
}
