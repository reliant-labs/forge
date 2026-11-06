package config

import "strings"

// NormalizeFrontendKind maps supported KCL and Expo aliases to inventory kinds.
// Unknown values are preserved; this is normalization, not validation or a
// capability registry. Callers still decide which kinds they support.
func NormalizeFrontendKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "nextjs":
		return "nextjs"
	case "vite", "vite-spa":
		return "vite-spa"
	case "rn", "react-native", "expo":
		return "react-native"
	default:
		return kind
	}
}
