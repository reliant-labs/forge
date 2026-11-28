package agentua

import "unsafe"

// unsafeString views b as a string without copying. Classify uses it on a
// stack buffer it never mutates afterwards and never lets escape, so the hot
// path lowercases a User-Agent with no heap allocation.
func unsafeString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}
