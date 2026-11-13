package obsquery

import (
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/observe"
)

// Who and what a user count leaves out. Two mechanisms, because the two
// questions differ:
//
//   - Events that are not the user's own activity. An emitter marks one as it
//     writes it (observe.TrackSystem, observe.SystemActivity, the web
//     runtime's track(name, props, {system: true})): NotSystemActivity, which
//     ActiveEvidence always applies. Events already stored cannot be
//     re-marked, so a project also names whole event types as system events
//     (UserFilter.SystemEvents): that is what corrects history.
//   - Users who are the app's own staff. forge does not know who they are,
//     and a product event never carries an email (the emitters refuse one),
//     so the app says it in the data: the users of a declared marker event
//     (one the app emits per staff member, e.g. a daily internal_user), and
//     users named by their opaque id (a synthetic monitor's account).
//
// Every app's own events still count by default: a zero UserFilter leaves
// ActiveEvidence as it is.

// NotSystemActivity is the predicate on product_events rows that excludes an
// event its emitter marked as not the user's own activity
// (Props['forge.activity'] = 'system'). An unmarked event reads ” there.
const NotSystemActivity = "Props['" + observe.PropActivity + "'] != '" + observe.ActivitySystem + "'"

// UserFilter is a project's declaration of the events and users its user
// counts leave out, beyond ActiveEvidence (forge.Analytics.system_events and
// forge.Analytics.staff in KCL). The zero value leaves nothing more out.
type UserFilter struct {
	// SystemEvents are product event names that are never a user's own
	// activity, wherever and whenever they were emitted.
	SystemEvents []string `json:",omitempty"`
	// StaffEvents are product event names that mark their UserId as staff:
	// a user with any one of them still retained (in the environments read)
	// is left out of every user count, on every day.
	StaffEvents []string `json:",omitempty"`
	// StaffUserIDs are opaque user ids (the events' UserId, never an email)
	// that are staff.
	StaffUserIDs []string `json:",omitempty"`
}

// IsZero reports whether f leaves nothing out.
func (f UserFilter) IsZero() bool {
	return len(f.SystemEvents) == 0 && len(f.StaffEvents) == 0 && len(f.StaffUserIDs) == 0
}

// Predicate is the SQL predicate on table's rows that f leaves in: not a
// declared system event, and not a staff user's. scope is an extra predicate
// on the staff marker rows ("" for none): the environments of the read, so
// a marker counts in the environments it was emitted in. "" when f is zero.
func (f UserFilter) Predicate(table, scope string) string {
	var conds []string
	if s := f.notSystem(); s != "" {
		conds = append(conds, s)
	}
	if s := f.notStaff(table, scope); s != "" {
		conds = append(conds, s)
	}
	return strings.Join(conds, " AND ")
}

// notStaff is the staff half of Predicate: the rows whose UserId is not
// staff. "" when f declares no staff.
func (f UserFilter) notStaff(table, scope string) string {
	var conds []string
	if ev := sortedSet(f.StaffEvents); len(ev) > 0 {
		marked := "EventName IN " + chTuple(ev) + " AND UserId != ''"
		if scope != "" {
			marked += " AND " + scope
		}
		conds = append(conds, "UserId NOT IN (SELECT UserId FROM "+table+" WHERE "+marked+")")
	}
	if ids := sortedSet(f.StaffUserIDs); len(ids) > 0 {
		conds = append(conds, "UserId NOT IN "+chTuple(ids))
	}
	return strings.Join(conds, " AND ")
}

func (f UserFilter) notSystem() string {
	ev := sortedSet(f.SystemEvents)
	if len(ev) == 0 {
		return ""
	}
	return "EventName NOT IN " + chTuple(ev)
}

// sortedSet is ss without empties and duplicates, sorted, so a declaration's
// order never changes the statement.
func sortedSet(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// chTuple is ss as a parenthesised list of ClickHouse string literals.
func chTuple(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = chStringLit(s)
	}
	return "(" + strings.Join(q, ", ") + ")"
}
