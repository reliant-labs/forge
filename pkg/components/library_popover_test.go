package components

import (
	"strings"
	"testing"
)

// The popover ships verbatim into every frontend. These guards pin how it
// decides "inside" for dismissal; the behaviour is exercised by the consuming
// app's tests (reliant:
// frontends/web/src/components/forge-ui/__tests__/popover.test.tsx).

// TestPopoverTreatsPortalledDescendantsAsInside: a control in the panel may
// portal its own layer to <body> (a nested popover, a full-screen picker
// sheet on touch), which is outside the panel's DOM but inside its React
// tree. React events bubble through portals, so the panel's capture handlers
// record presses and focus there, and the native outside-press and focusout
// listeners skip the close when they did (focusout hands the decision to the
// document focusin that follows, so the close stays synchronous) — otherwise a tap on the portalled
// layer dismisses the panel and unmounts the control that opened it.
func TestPopoverTreatsPortalledDescendantsAsInside(t *testing.T) {
	t.Parallel()
	src, err := NewLibrary().Get("popover")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`onPointerDownCapture={() => {`,
		`pressInTree.current = true;`,
		`onFocusCapture={() => {`,
		`focusInTree.current = true;`,
		// the document pointerdown listener reads and resets the record
		`const inTree = pressInTree.current;`,
		`if (inTree || triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;`,
		// focusout only marks that focus left; the document focusin that follows,
		// which runs after React's listeners, decides synchronously
		`focusLeft = true;`,
		`document.addEventListener("focusin", onFocusIn);`,
		`if (!inTree && !triggerRef.current?.contains(document.activeElement)) close(false);`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("popover.tsx no longer contains %q", want)
		}
	}
}
