package naming

import (
	"sort"
	"strings"
	"unicode"
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
	"HTML", "HTTP", "HTTPS", "ID", "IP", "JSON", "LHS", "MCP", "QPS",
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