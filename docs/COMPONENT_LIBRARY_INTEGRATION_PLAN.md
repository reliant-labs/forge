# Component Library → Scaffold Integration Plan

## Overview

When `forge new` scaffolds a Next.js frontend, it should install relevant components from the component library into `src/components/` and the generated CRUD pages should import and use them — replacing the current pattern of inlining all UI in page templates.

## Current State

- **Component library**: 61 components in `components/components/` (embedded via `//go:embed`)
- **Page templates**: `internal/templates/frontend/pages/{list,detail,create,edit}-page.tsx.tmpl`
- **Scaffold flow**: `forge new` → `GenerateFrontendFiles()` → static Next.js skeleton; then `forge generate` → `generateFrontendPages()` → CRUD pages per entity
- **Problem**: Page templates inline all their UI (loading skeletons, badges, empty states, modals, form fields, pagination) — duplicated across every page, not reusable

## Design

### 1. Auto-install core components during `forge new`

When `GenerateFrontendFiles()` runs, it writes the Next.js skeleton **plus** a set of base components into `frontends/<name>/src/components/ui/`.

**Always-installed components** (the "core kit"):
- `page_header.tsx` — used on every page
- `badge.tsx` — status display in tables/detail views
- `modal.tsx` — delete confirmation, create forms
- `skeleton_loader.tsx` — loading states everywhere
- `pagination.tsx` — list pages
- `search_input.tsx` — list filtering
- `alert_banner.tsx` — error/success messages
- `toast_notification.tsx` — action feedback
- `key_value_list.tsx` — detail view field display
- `tabs.tsx` — detail view sections

**Implementation** — add to `internal/generator/frontend_gen.go`:

```go
// After writing the nextjs skeleton files, install core components
func installCoreComponents(frontendDir string) error {
    lib := components.NewLibrary()
    
    coreComponents := []string{
        "page_header", "badge", "modal", "skeleton_loader",
        "pagination", "search_input", "alert_banner",
        "toast_notification", "key_value_list", "tabs",
    }
    
    componentsDir := filepath.Join(frontendDir, "src", "components", "ui")
    if err := os.MkdirAll(componentsDir, 0755); err != nil {
        return err
    }
    
    for _, name := range coreComponents {
        content, err := lib.Get(name)
        if err != nil {
            return fmt.Errorf("get component %s: %w", name, err)
        }
        dest := filepath.Join(componentsDir, name+".tsx")
        if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
            return fmt.Errorf("write component %s: %w", name, err)
        }
    }
    return nil
}
```

Call from `GenerateFrontendFiles()`:
```go
func GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int) error {
    // ... existing skeleton file writing ...
    
    // Install core UI components
    frontendDir := filepath.Join(root, "frontends", frontendName)
    if err := installCoreComponents(frontendDir); err != nil {
        return fmt.Errorf("install core components: %w", err)
    }
    
    return nil
}
```

### 2. Page templates import installed components

Rewrite the 4 page templates to import from `@/components/ui/` instead of inlining everything.

#### `list-page.tsx.tmpl` (before/after sketch)

**Before** (current): Inline `LoadingSkeleton`, `Badge`, `EmptyState`, full table rendering — ~165 lines

**After**:
```tsx
"use client";

import { use{{.ListRPC}} } from "{{.HooksImportPath}}";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useState } from "react";
import PageHeader from "@/components/ui/page_header";
import Badge from "@/components/ui/badge";
import SkeletonLoader from "@/components/ui/skeleton_loader";
import Pagination from "@/components/ui/pagination";
import SearchInput from "@/components/ui/search_input";

export default function {{.EntityNamePlural}}Page() {
  const router = useRouter();
  const [search, setSearch] = useState("");
  const { data, isLoading, error } = use{{.ListRPC}}({});

  if (isLoading) return <SkeletonLoader variant="table-row" count={5} />;
  // ... rest uses PageHeader, Badge, Pagination, SearchInput
}
```

#### `detail-page.tsx.tmpl`
Import: `PageHeader`, `Badge`, `KeyValueList`, `Modal` (for delete confirm), `SkeletonLoader`

#### `create-page.tsx.tmpl` / `edit-page.tsx.tmpl`
Import: `PageHeader`, `AlertBanner` (for validation errors), `Modal`

### 3. `forge component install <name>` CLI command

Add a CLI command for installing additional components on demand:

```
forge component install metric_card activity_feed avatar
forge component list
forge component search "chart dashboard"
```

**File**: `internal/cli/component.go`

```go
func newComponentCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "component",
        Short: "Manage UI components from the component library",
    }
    cmd.AddCommand(newComponentInstallCmd())
    cmd.AddCommand(newComponentListCmd())
    cmd.AddCommand(newComponentSearchCmd())
    return cmd
}

func newComponentInstallCmd() *cobra.Command {
    var targetDir string
    cmd := &cobra.Command{
        Use:   "install <component-names...>",
        Short: "Install components into your project",
        Args:  cobra.MinimumNArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            lib := components.NewLibrary()
            dir := targetDir
            if dir == "" {
                // Auto-detect: look for src/components/ui/ in the nearest frontend
                dir = detectComponentsDir()
            }
            for _, name := range args {
                content, err := lib.Get(name)
                if err != nil {
                    return err
                }
                dest := filepath.Join(dir, name+".tsx")
                os.MkdirAll(filepath.Dir(dest), 0755)
                os.WriteFile(dest, []byte(content), 0644)
                fmt.Printf("  ✅ Installed %s → %s\n", name, dest)
            }
            return nil
        },
    }
    cmd.Flags().StringVarP(&targetDir, "dir", "d", "", "target directory (default: auto-detect)")
    return cmd
}
```

### 4. How this affects the template system

**Changes required**:

| File | Change |
|------|--------|
| `internal/generator/frontend_gen.go` | Add `installCoreComponents()` call |
| `internal/templates/frontend/pages/list-page.tsx.tmpl` | Rewrite to import components |
| `internal/templates/frontend/pages/detail-page.tsx.tmpl` | Rewrite to import components |
| `internal/templates/frontend/pages/create-page.tsx.tmpl` | Rewrite to import components |
| `internal/templates/frontend/pages/edit-page.tsx.tmpl` | Rewrite to import components |
| `internal/cli/component.go` | New file — CLI commands |
| `internal/cli/root.go` | Register `component` subcommand |

**No changes needed**:
- Component library itself (`components/`) — already works
- MCP tool in Reliant — `install` action already handles this for LLM usage
- Nav template — stays as-is
- Hooks/mock generation — stays as-is

### 5. Component dependency awareness

Some components depend on others conceptually (e.g., `page_header` uses breadcrumb patterns). We do NOT add explicit import dependencies between library components — each is self-contained. If a page template needs to compose multiple components, the template handles the composition.

### 6. Upgrade path for existing projects

For existing projects that already have generated pages:
- `forge generate` re-generates pages, so running it after this change will automatically switch to the component-based templates
- Components that don't exist yet in `src/components/ui/` get installed automatically during `forge generate` (add a check to `generateFrontendPages()`)
- No migration needed — it's a clean re-gen

### 7. File layout after scaffold

```
frontends/dashboard/
├── src/
│   ├── app/
│   │   ├── page.tsx              # Dashboard home
│   │   ├── users/
│   │   │   ├── page.tsx          # List (imports PageHeader, Badge, Pagination, etc.)
│   │   │   ├── [id]/
│   │   │   │   ├── page.tsx      # Detail (imports PageHeader, KeyValueList, etc.)
│   │   │   │   └── edit/
│   │   │   │       └── page.tsx  # Edit (imports PageHeader, AlertBanner)
│   │   │   └── new/
│   │   │       └── page.tsx      # Create (imports PageHeader, AlertBanner)
│   │   └── layout.tsx
│   ├── components/
│   │   ├── nav.tsx               # Generated nav (existing)
│   │   └── ui/                   # Auto-installed from component library
│   │       ├── page_header.tsx
│   │       ├── badge.tsx
│   │       ├── modal.tsx
│   │       ├── skeleton_loader.tsx
│   │       ├── pagination.tsx
│   │       ├── search_input.tsx
│   │       ├── alert_banner.tsx
│   │       ├── toast_notification.tsx
│   │       ├── key_value_list.tsx
│   │       └── tabs.tsx
│   ├── hooks/                    # Generated hooks (existing)
│   └── lib/                      # Generated lib (existing)
```

## Implementation Order

1. Add `installCoreComponents()` to `frontend_gen.go`
2. Rewrite the 4 page templates to use imports
3. Add component ensure step to `generateFrontendPages()` (install missing components)
4. Add `forge component` CLI commands
5. Update tests (`scaffold_full_e2e_test.go`, etc.)
6. Update docs/CHANGELOG
