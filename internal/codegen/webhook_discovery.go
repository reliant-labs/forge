package codegen

import (
	"os"
	"sort"
	"strings"
)

// WebhookNamesForService discovers a service's webhooks from the REAL
// source — the webhook_<name>.go handler files under its directory — rather
// than a declared config list. `forge add webhook` scaffolds
// webhook_<name>.go (+ _test.go + a shared webhook_store.go); the file IS
// the declaration, so nothing needs to cache the name in forge.yaml or a
// components manifest. This mirrors how forge discovers services themselves.
//
// handlerDir is the service's on-disk directory (e.g. internal/handlers/foo,
// resolved via ResolveServiceComponent). Returns the webhook names sorted for
// stable codegen; webhook_store.go and *_test.go are excluded. Best-effort: a
// missing directory yields nil.
func WebhookNamesForService(handlerDir string) []string {
	entries, err := os.ReadDir(handlerDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(n, "webhook_") || !strings.HasSuffix(n, ".go") {
			continue
		}
		if n == "webhook_store.go" || strings.HasSuffix(n, "_test.go") {
			continue
		}
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(n, "webhook_"), ".go"))
	}
	sort.Strings(names)
	return names
}

// ServiceHasWebhooks reports whether a service's directory carries any
// webhook_<name>.go handler file.
func ServiceHasWebhooks(handlerDir string) bool {
	return len(WebhookNamesForService(handlerDir)) > 0
}
