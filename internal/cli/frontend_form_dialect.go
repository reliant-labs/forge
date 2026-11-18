package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// zod4MigrationSkill is the playbook that moves a frontend from zod 3 to
// zod 4. `forge generate` names it when it writes a page for zod 3, and
// TestZod4Migration_Detection fails if it is not shipped.
const zod4MigrationSkill = "migrations/v1.8.0"

// frontendFormDialect reads the zod and @hookform/resolvers majors the
// frontend at frontendAbsDir declares, and answers the form dialect its
// generated create/edit pages must be written in.
//
// The frontend's package.json is its owner's: a frontend scaffolded before
// forge moved to zod 4 still declares zod 3 until the owner bumps it
// (migrations/v1.8.0). A page forge adds to it in the meantime — a new
// entity's create form — has to compile against what it declares.
//
// Anything forge cannot read as a version (no package.json, no such
// dependency, `workspace:*`, `catalog:`) is today's default: zod 4 with a
// resolver from 5 on.
func frontendFormDialect(frontendAbsDir string) codegen.FormDialect {
	raw, err := os.ReadFile(filepath.Join(frontendAbsDir, "package.json"))
	if err != nil {
		return codegen.FormDialect{}
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(raw, &pkg) != nil {
		return codegen.FormDialect{}
	}
	declared := func(name string) string {
		if v, ok := pkg.Dependencies[name]; ok {
			return v
		}
		return pkg.DevDependencies[name]
	}
	zod, zodOK := declaredMajor(declared("zod"))
	resolvers, resolversOK := declaredMajor(declared("@hookform/resolvers"))
	return codegen.FormDialect{
		Zod3:           zodOK && zod < 4,
		LegacyResolver: resolversOK && resolvers < 5,
	}
}

// declaredMajor reads the major version a package.json range starts from:
// "^3.23.0", "~3.25", ">=3.0.0", "3.x" and "v4" all have one. It reports
// false for a range with no leading version (`workspace:*`, `catalog:`,
// `latest`, a git or file spec).
func declaredMajor(spec string) (int, bool) {
	s := strings.TrimLeft(strings.TrimSpace(spec), "^~>=v ")
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	major, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return major, true
}
