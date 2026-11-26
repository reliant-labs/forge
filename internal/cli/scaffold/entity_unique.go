// File: internal/cli/scaffold/entity_unique.go
//
// The birth pre-flight and post-birth report for `// forge:unique <cols>`
// message markers (codegen.ProtoMarkerUnique).
//
// Before a birth writes anything, every reason a key cannot become the
// UNIQUE index its author wrote is found here and reported against the
// marker's file and line: a marker birth does not read (inside a body,
// trailing a field), a near-miss spelling, a key naming no column, a column
// the table is not born with, a repeated key. A key dropped in silence is a
// constraint the author believes postgres enforces, so each of these refuses
// the birth rather than warning.
//
// After birth the marker is inert like every birth marker, but a key ADDED
// to an already-born message is still a gap the author asked to close. The
// sweep reports it with the index to write in a new migration — it never
// writes one, because no command derives a migration from proto state after
// birth.

package scaffold

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
	entityscaffold "github.com/reliant-labs/forge/internal/scaffold"
	"github.com/reliant-labs/forge/pkg/schemadef"
)

// uniqueKeysOf projects the raw scan's markers onto the renderer's keys.
func uniqueKeysOf(markers []codegen.UniqueKeyMarker) []entityscaffold.UniqueKey {
	if len(markers) == 0 {
		return nil
	}
	out := make([]entityscaffold.UniqueKey, 0, len(markers))
	for _, m := range markers {
		out = append(out, entityscaffold.UniqueKey{Columns: m.Columns})
	}
	return out
}

// uniqueMarkerRefusal refuses a birth whose message carries a forge:unique
// marker the birth cannot honour. spec is the exact spec the birth renders;
// m is the raw-scanned message, which carries the markers and their sites
// (the zero value when the scan never saw the message — nothing to refuse).
func uniqueMarkerRefusal(spec entityscaffold.EntityFromProtoSpec, m codegen.RawProtoMessage) error {
	var problems []string
	for _, site := range m.UnappliedUniqueMarkers {
		problems = append(problems, fmt.Sprintf("%s\n      is not where birth reads a key — put it on its own comment line directly above `message %s {`", site, m.Name))
	}
	for _, site := range m.UniqueMarkerNearMisses {
		problems = append(problems, fmt.Sprintf("%s\n      looks like a misspelling of %s — as written it births no key", site, codegen.ProtoMarkerUnique))
	}
	seen := map[string]codegen.RawProtoMarkerSite{}
	for _, um := range m.Unique {
		if reason := entityscaffold.UniqueKeyRefusal(entityscaffold.UniqueKey{Columns: um.Columns}, spec); reason != "" {
			problems = append(problems, fmt.Sprintf("%s\n      %s %s", um.Site, codegen.ProtoMarkerUnique, reason))
			continue
		}
		set := keySetID(um.Columns)
		if first, dup := seen[set]; dup {
			problems = append(problems, fmt.Sprintf("%s\n      repeats the key declared at %s — keep one", um.Site, first))
			continue
		}
		seen[set] = um.Site
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(messageLabel(spec) + " carries a " + codegen.ProtoMarkerUnique +
		" marker birth cannot honour (nothing was written):\n    " + strings.Join(problems, "\n    "))
}

// keySetID identifies a key by its column SET: uniqueness over (a, b) is
// uniqueness over (b, a), so two markers naming the same columns in either
// order are one key declared twice.
func keySetID(cols []string) string {
	s := append([]string(nil), cols...)
	sort.Strings(s)
	return strings.Join(s, ",")
}

// uniqueDriftNotes reports, for every already-born message in scan, each
// forge:unique key the applied table has no unique index for — the marker
// was added after birth, so nothing will ever build it. One note per key,
// naming the statement to write in a new migration.
//
// tables must come from real introspection: the regex fallback carries no
// indexes, and every key would read as missing. A table holding the same
// column set under any unique index counts, whatever its predicate or column
// order — the author chose that shape in a migration they own.
func uniqueDriftNotes(scan *codegen.RawProtoScan, tables []schemadef.Table) []string {
	if scan == nil || len(tables) == 0 {
		return nil
	}
	byName := make(map[string]schemadef.Table, len(tables))
	for _, t := range tables {
		byName[t.Name] = t
	}
	var notes []string
	for _, m := range scan.Messages {
		if len(m.Unique) == 0 {
			continue
		}
		table, ok := byName[naming.Pluralize(naming.ToSnakeCase(m.Name))]
		if !ok {
			continue // not born yet: the birth itself emits (or refuses) the keys
		}
		have := map[string]bool{}
		for _, ix := range table.Indexes {
			if ix.Unique && !ix.Expression {
				have[keySetID(ix.Columns)] = true
			}
		}
		for _, um := range m.Unique {
			if len(um.Columns) == 0 || have[keySetID(um.Columns)] {
				continue
			}
			key := entityscaffold.UniqueKey{Columns: um.Columns}
			notes = append(notes, fmt.Sprintf(
				"%s: %s (%s) at %s is not in the applied schema — %q was born before it, and birth is one-time. "+
					"Add it in a new migration: CREATE UNIQUE INDEX %s ON %s (%s); "+
					"(on a table with live traffic, `forge lint --migration-safety` says how to build it CONCURRENTLY)",
				m.Name, codegen.ProtoMarkerUnique, strings.Join(um.Columns, ", "), um.Site, table.Name,
				entityscaffold.UniqueKeyIndexName(table.Name, key), table.Name, strings.Join(um.Columns, ", ")))
		}
	}
	return notes
}
