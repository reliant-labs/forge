//go:build ignore

package generator

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// GeneratePlanContract writes a contract.go file into pkgDir with populated
// Service interface methods and DTO struct types derived from the plan.
func GeneratePlanContract(pkgDir, pkgName string, methods []config.PlanMethod, types []config.PlanType) error {
	var buf bytes.Buffer

	buf.WriteString("package " + pkgName + "\n\n")

	// Collect imports from method signatures and field types.
	imports := collectPlanImports(methods, types)
	if len(imports) > 0 {
		buf.WriteString("import (\n")
		for _, imp := range imports {
			fmt.Fprintf(&buf, "\t%q\n", imp)
		}
		buf.WriteString(")\n\n")
	}

	// Service interface.
	buf.WriteString("// Service defines the " + pkgName + " package boundary.\n")
	buf.WriteString("type Service interface {\n")
	for _, m := range methods {
		fmt.Fprintf(&buf, "\t%s(%s)", m.Name, m.Args)
		if m.Returns != "" {
			ret := m.Returns
			if strings.Contains(ret, ",") {
				ret = "(" + ret + ")"
			}
			fmt.Fprintf(&buf, " %s", ret)
		}
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")

	// DTO types.
	for _, t := range types {
		typeName := stripPackagePrefix(pkgName, t.Name)
		buf.WriteString("\n")
		if len(t.Fields) == 0 {
			// No fields → enum placeholder type.
			fmt.Fprintf(&buf, "// %s is a type placeholder (e.g. for enums).\n", typeName)
			fmt.Fprintf(&buf, "type %s string\n", typeName)
		} else {
			fmt.Fprintf(&buf, "// %s is a DTO for the %s package.\n", typeName, pkgName)
			fmt.Fprintf(&buf, "type %s struct {\n", typeName)
			for _, f := range t.Fields {
				jsonTag := f.JSON
				if jsonTag == "" {
					jsonTag = naming.ToSnakeCase(f.Name)
				}
				fmt.Fprintf(&buf, "\t%s %s `json:\"%s\"`\n", f.Name, f.Type, jsonTag)
			}
			buf.WriteString("}\n")
		}
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("gofmt plan contract: %w\n---\n%s", err, buf.String())
	}

	return os.WriteFile(filepath.Join(pkgDir, "contract.go"), formatted, 0644)
}

// stripPackagePrefix removes a stuttering package-name prefix from a type name.
// e.g. stripPackagePrefix("billing", "BillingInvoice") → "Invoice"
// e.g. stripPackagePrefix("billing", "Invoice") → "Invoice" (no change)
func stripPackagePrefix(pkgName, typeName string) string {
	prefix := strings.ToUpper(pkgName[:1]) + pkgName[1:]
	if strings.HasPrefix(typeName, prefix) {
		trimmed := strings.TrimPrefix(typeName, prefix)
		if len(trimmed) > 0 && unicode.IsUpper(rune(trimmed[0])) {
			return trimmed
		}
	}
	return typeName
}

// collectPlanImports scans method signatures and field types for package
// references like "context.Context" or "time.Time" and returns the sorted
// list of import paths that need to be included.
func collectPlanImports(methods []config.PlanMethod, types []config.PlanType) []string {
	// Map of package short name → standard-library import path.
	// Extend as needed for commonly used stdlib packages.
	knownPackages := map[string]string{
		"context": "context",
		"time":    "time",
		"sql":     "database/sql",
		"fmt":     "fmt",
		"io":      "io",
		"net":     "net",
		"http":    "net/http",
		"json":    "encoding/json",
		"bytes":   "bytes",
		"strings": "strings",
		"errors":  "errors",
		"slog":    "log/slog",
		"sync":    "sync",
	}

	needed := make(map[string]bool)

	scanStr := func(s string) {
		for alias, imp := range knownPackages {
			if strings.Contains(s, alias+".") {
				needed[imp] = true
			}
		}
	}

	for _, m := range methods {
		scanStr(m.Args)
		scanStr(m.Returns)
	}
	for _, t := range types {
		for _, f := range t.Fields {
			scanStr(f.Type)
		}
	}

	var imports []string
	for imp := range needed {
		imports = append(imports, imp)
	}
	sort.Strings(imports)
	return imports
}