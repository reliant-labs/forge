package naming

import (
	"sort"
	"strings"
	"unicode"

	"github.com/jinzhu/inflection"
)

// ToPascalCase converts a hyphenated, underscored, or camelCase name to PascalCase.
// It handles both '-' and '_' as word separators and capitalizes Go initialisms.
// e.g. "api-gateway" -> "APIGateway", "user_service" -> "UserService", "http_client" -> "HTTPClient".
func ToPascalCase(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range s {
		if r == '-' || r == '_' {
			upNext = true
			continue
		}
		if upNext {
			b.WriteRune(unicode.ToUpper(r))
			upNext = false
		} else {
			b.WriteRune(r)
		}
	}
	return applyGoInitialisms(b.String())
}

// GoInitialisms are common Go initialisms that should be all-caps.
// This is the single source of truth — all packages must import from here.
var GoInitialisms = []string{
	"ACL", "API", "ASCII", "CPU", "CSS", "DB", "DNS", "EOF", "GUID",
	"HTML", "HTTP", "HTTPS", "ID", "IO", "IP", "JSON", "JWT", "LHS",
	"LLM", "MCP", "QPS",
	"RAM", "RHS", "RPC", "SLA", "SMTP", "SQL", "SSH", "TCP",
	"TLS", "TTL", "UDP", "UI", "UID", "UUID", "URI", "URL",
	"UTF8", "VM", "XML", "XMPP", "XSRF", "XSS",
}

// GoInitialismsMap provides O(1) lookup for initialism detection (lowercase keys).
var GoInitialismsMap = func() map[string]bool {
	m := make(map[string]bool, len(GoInitialisms))
	for _, v := range GoInitialisms {
		m[strings.ToLower(v)] = true
	}
	return m
}()

// goInitialismsSorted is GoInitialisms sorted by length descending,
// so that longer initialisms (e.g. "UUID") are replaced before shorter
// ones (e.g. "ID") to avoid partial replacements like "UuID".
var goInitialismsSorted = func() []string {
	sorted := make([]string, len(GoInitialisms))
	copy(sorted, GoInitialisms)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i]) > len(sorted[j])
	})
	return sorted
}()

// applyGoInitialisms replaces known initialisms with their all-caps form.
// e.g. "HttpClient" -> "HTTPClient", "ApiId" -> "APIID".
// Processes longer initialisms first to avoid partial replacements.
func applyGoInitialisms(s string) string {
	for _, initialism := range goInitialismsSorted {
		titleCase := strings.ToUpper(initialism[:1]) + strings.ToLower(initialism[1:])
		s = strings.ReplaceAll(s, titleCase, initialism)
	}
	return s
}

// ToSnakeCase converts a string (camelCase, PascalCase, UPPER_CASE, etc.) to snake_case.
// e.g. "firstName" -> "first_name", "HTTPStatus" -> "http_status", "UPPER_CASE" -> "upper_case".
func ToSnakeCase(s string) string {
	// Collect runes up front so we can look ahead one rune without reindexing
	// into the byte slice (which would be unsafe for multi-byte code points).
	runes := []rune(s)
	var b strings.Builder
	var prev rune
	for i, r := range runes {
		if unicode.IsUpper(r) {
			if i > 0 {
				// Insert underscore before an uppercase letter if preceded by a lowercase letter,
				// or if preceded by an uppercase letter followed by a lowercase letter (e.g. "HTTPStatus" -> "http_status").
				if unicode.IsLower(prev) {
					b.WriteByte('_')
				} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
					b.WriteByte('_')
				}
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
		prev = r
	}
	return b.String()
}

// Pluralize returns the English plural form of a word using the inflection library.
func Pluralize(s string) string {
	if len(s) == 0 {
		return s
	}
	return inflection.Plural(s)
}

// ToProtoPascalCase converts a snake_case proto field name to PascalCase using
// protobuf's Go naming rules: simple title-case each word segment WITHOUT
// applying Go initialisms. For example:
//   - "id" → "Id" (not "ID")
//   - "org_id" → "OrgId" (not "OrgID")
//   - "http_status" → "HttpStatus" (not "HTTPStatus")
//
// This matches the field names that protoc-gen-go actually generates.
func ToProtoPascalCase(s string) string {
	var b strings.Builder
	upNext := true
	for _, r := range s {
		if r == '-' || r == '_' {
			upNext = true
			continue
		}
		if upNext {
			b.WriteRune(unicode.ToUpper(r))
			upNext = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsGoInitialism reports whether word (case-insensitive) is a known Go initialism.
func IsGoInitialism(word string) bool {
	return GoInitialismsMap[strings.ToLower(word)]
}

// ToKebabCase converts a name to kebab-case, treating known initialisms
// (LLM, API, URL, JSON, etc. — see GoInitialisms) as a single segment so
// that "LLMGateway" → "llm-gateway" rather than "l-l-m-gateway".
//
// Accepts input in PascalCase, camelCase, snake_case, or already-kebab-
// case form; the output is always lowercase, hyphen-separated, with no
// runs of multiple hyphens. This is the canonical kebab function for
// every site that emits filenames, slugs, route paths, or import paths
// that need to round-trip with proto Go names — keep it the single
// source of truth so frontend hooks files, navigation slugs, and re-
// export indexers can never disagree on whether "LLMGateway" splits as
// "l-l-m-gateway" or "llm-gateway".
func ToKebabCase(s string) string {
	if s == "" {
		return ""
	}

	// Two-stage pipeline:
	//   1. Insert separators at every word boundary, recognising known
	//      initialisms as a single word.
	//   2. Lowercase + collapse runs of separators into a single '-'.
	runes := []rune(s)
	var segments []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			segments = append(segments, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}

	i := 0
	for i < len(runes) {
		r := runes[i]
		if r == '_' || r == '-' || r == ' ' {
			flush()
			i++
			continue
		}

		// At an uppercase rune: see if a known initialism starts here.
		if unicode.IsUpper(r) {
			// Length of the contiguous uppercase run starting at i.
			runEnd := i
			for runEnd < len(runes) && unicode.IsUpper(runes[runEnd]) {
				runEnd++
			}
			runLen := runEnd - i

			matchedLen := 0
			for _, init := range goInitialismsSorted {
				il := len(init)
				if il > runLen {
					continue
				}
				// Compare runes[i:i+il] to init case-insensitively.
				ok := true
				for k := 0; k < il; k++ {
					if !unicode.IsUpper(runes[i+k]) {
						ok = false
						break
					}
					if toLowerByte(byte(runes[i+k])) != toLowerByte(init[k]) {
						ok = false
						break
					}
				}
				if !ok {
					continue
				}
				// When the initialism is shorter than the uppercase run,
				// only treat it as a boundary if the next rune begins a
				// lowercase word ("HTTPSConnection" → HTTPS + Connection).
				// Otherwise we'd split "AAPI" into "a"+"api"; better to
				// fall through to the standard PascalCase rule and let
				// the run remain glued.
				if il < runLen && i+il < len(runes) && !unicode.IsLower(runes[i+il]) {
					continue
				}
				matchedLen = il
				break
			}

			if matchedLen > 0 {
				flush()
				for k := 0; k < matchedLen; k++ {
					cur.WriteRune(runes[i+k])
				}
				flush()
				i += matchedLen
				continue
			}

			// Standard PascalCase split: any uppercase rune begins a
			// new segment unless it's part of an existing uppercase run
			// of length > 1 (e.g. "HTTPClient" → "HTTP" then "Client",
			// where "P" is the last uppercase before "C" lowercase'd).
			// The "look ahead one rune for lowercase" rule lets the
			// run of caps end exactly where a CamelCase word begins.
			flush()
			cur.WriteRune(r)
			i++
			for i < len(runes) && unicode.IsUpper(runes[i]) {
				// If the NEXT-NEXT rune is lowercase, THIS uppercase
				// is the start of the next CamelCase word — flush.
				if i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
					break
				}
				cur.WriteRune(runes[i])
				i++
			}
			continue
		}

		// Lowercase / digit / other — accumulate into the current segment.
		cur.WriteRune(r)
		i++
	}
	flush()

	out := make([]string, 0, len(segments))
	for _, seg := range segments {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return strings.Join(out, "-")
}

func toLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// ToExportedFieldName converts a lowercase package/field name to an exported
// Go identifier, respecting Go initialisms.
// Examples: "api" -> "API", "db" -> "DB", "orders" -> "Orders"
func ToExportedFieldName(pkg string) string {
	if len(pkg) == 0 {
		return pkg
	}
	if GoInitialismsMap[strings.ToLower(pkg)] {
		return strings.ToUpper(pkg)
	}
	return strings.ToUpper(pkg[:1]) + pkg[1:]
}