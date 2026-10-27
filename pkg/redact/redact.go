// Package redact removes credential shapes and personal paths from text that
// leaves the process: a bug report, a log line, an error string sent to
// telemetry.
//
// It is pattern-based, so it is a backstop, not a guarantee: it catches the
// shapes credentials are usually written in (URL userinfo, key=value pairs
// with a credential-named key, bearer and basic schemes, JWTs, the key
// prefixes of the common providers, long opaque runs) and home directories,
// which name the person. Text that must never leave the process must not be
// handed to anything that sends it, redacted or not.
//
// The credential patterns are the ones reliant redacts its agent bug reports
// with (reliant internal/telemetry), held here so forge, reliant and the
// control plane share one bar.
//
//forge:exclude-contract: a pure-function library over strings, stdlib only, with nothing to substitute
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// Marker replaces a redacted credential.
	Marker = "[redacted]"
	// TruncatedMarker ends text Bounded cut.
	TruncatedMarker = " [truncated]"
)

type pattern struct {
	re   *regexp.Regexp
	repl string
}

// secretPatterns are the credential shapes Secrets removes, in order: an
// earlier pattern's replacement is never re-matched by a later one.
var secretPatterns = []pattern{
	// URL userinfo: postgres://user:password@host
	{regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^\s/@:]+:[^\s/@]+@`), "${1}" + Marker + "@"},
	// key=value / "key": "value" for credential-named keys, including an
	// Authorization header's scheme so the whole credential goes at once.
	{regexp.MustCompile(`(?i)\b(authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|secret|password|passwd|token)(["']?\s*[:=]\s*["']?)(?:(?:bearer|basic|token)\s+)?[^\s"'&,;]+`), "${1}${2}" + Marker},
	// A bare authorization scheme outside a key=value pair.
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]+`), "${1} " + Marker},
	// JWTs.
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]*`), Marker},
	// Provider and platform key prefixes.
	{regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_\-]{8,}|sk-[A-Za-z0-9_\-]{16,}|sk_(?:live|test)_[A-Za-z0-9]{8,}|(?:rlat|dpat|gho|ghp|ghu|ghs|ghr|github_pat|glpat|xox[abprs])[_\-][A-Za-z0-9_\-]{10,}|AIza[0-9A-Za-z_\-]{20,}|ya29\.[0-9A-Za-z_\-]+|AKIA[0-9A-Z]{16})`), Marker},
	// Long opaque runs (digests, base64 blobs). '-', '/' and '.' are excluded
	// so UUIDs, paths and dotted names are not swallowed.
	{regexp.MustCompile(`[A-Za-z0-9+=_]{40,}`), Marker},
}

// homePatterns are the home-directory prefixes HomeDirs replaces with "~".
var homePatterns = []pattern{
	{regexp.MustCompile(`(/home/|/Users/)[^/\s"':]+`), "~"},
	{regexp.MustCompile(`(?i)\b[A-Z]:\\Users\\[^\\\s"':]+`), "~"},
}

// Secrets replaces every credential shape in s with Marker.
func Secrets(s string) string {
	return apply(s, secretPatterns)
}

// HomeDirs replaces every home directory in s (/home/<user>, /Users/<user>,
// C:\Users\<user>) with "~", keeping the rest of each path, so text still
// says which file without saying whose.
func HomeDirs(s string) string {
	return apply(s, homePatterns)
}

// Text is Secrets and HomeDirs: what text bound for someone else's eyes
// needs.
func Text(s string) string {
	return HomeDirs(Secrets(s))
}

// Bounded is Text, cut to at most maxBytes UTF-8 bytes, including
// TruncatedMarker. Invalid UTF-8 is replaced before measuring; cuts never
// split a rune. A bound smaller than the marker retains only its ASCII
// prefix. maxBytes <= 0 means no bound. JSON escaping may expand the encoded
// representation; this bounds the decoded text, not the enclosing JSON.
func Bounded(s string, maxBytes int) string {
	s = Text(strings.ToValidUTF8(s, "\uFFFD"))
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	if maxBytes <= len(TruncatedMarker) {
		return TruncatedMarker[:maxBytes]
	}
	end := maxBytes - len(TruncatedMarker)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + TruncatedMarker
}

func apply(s string, patterns []pattern) string {
	for _, p := range patterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
