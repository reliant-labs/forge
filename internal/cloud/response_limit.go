package cloud

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// DefaultMaxResponseBytes is the most bytes of a reply a Client reads for a
// procedure with no declared limit (DeclareResponseLimit).
const DefaultMaxResponseBytes = 8 << 20

// ErrResponseTooLarge is matched (errors.Is) by every *ResponseTooLargeError.
var ErrResponseTooLarge = errors.New("control-plane response over the read limit")

// ResponseTooLargeError is a successful reply longer than the client reads
// for its procedure. The reply is not decoded: a prefix of a JSON document is
// not a smaller answer, it is no answer.
type ResponseTooLargeError struct {
	Procedure string
	Limit     int64
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("the control plane's %s answer is over %s, more than forge reads",
		e.Procedure[strings.LastIndex(e.Procedure, "/")+1:], byteSize(e.Limit))
}

// Is makes errors.Is(err, ErrResponseTooLarge) hold.
func (e *ResponseTooLargeError) Is(target error) bool { return target == ErrResponseTooLarge }

func byteSize(n int64) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// DeclareResponseLimit says that a reply to each of procedures may be up to
// limit bytes, in place of DefaultMaxResponseBytes.
//
// WHY PER PROCEDURE. The default bounds what a misbehaving endpoint (an HTML
// error page, a proxy streaming garbage) can make forge hold in memory, and it
// is right for nearly every procedure. A few answer with documents that are
// legitimately bigger: GetTrace returns up to 10,000 spans with all their
// attributes, which is well over 8 MiB for a long request. Those callers know
// their procedure's bound, so they say so where they name it, and every other
// procedure keeps the tight default. A limit <= 0 restores the default.
func (c *Client) DeclareResponseLimit(limit int64, procedures ...string) {
	for _, procedure := range procedures {
		c.responseLimits.set(procedure, limit)
	}
}

// responseLimitTable maps a procedure to its declared read limit.
type responseLimitTable struct {
	mu     sync.Mutex
	byProc map[string]int64
}

func (t *responseLimitTable) set(procedure string, limit int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	procedure = strings.TrimPrefix(procedure, "/")
	if limit <= 0 {
		delete(t.byProc, procedure)
		return
	}
	if t.byProc == nil {
		t.byProc = map[string]int64{}
	}
	t.byProc[procedure] = limit
}

func (t *responseLimitTable) limit(procedure string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n, ok := t.byProc[strings.TrimPrefix(procedure, "/")]; ok {
		return n
	}
	return DefaultMaxResponseBytes
}
