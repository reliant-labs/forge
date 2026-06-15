package projectstore

import "strings"

func trim(s string) string      { return strings.TrimSpace(s) }
func lowerTrim(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
