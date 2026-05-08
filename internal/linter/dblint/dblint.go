// Package dblint provides lint rules for proto-defined database entities.
// Rules are advisory (warnings) and check for missing best practices in
// entity proto definitions found in proto/db/.
package dblint

import (
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/database"
)

// Severity indicates how important a finding is.
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Finding represents a single lint finding.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Entity   string   `json:"entity"`
	Field    string   `json:"field,omitempty"`
}

// Result holds all findings from a lint run.
type Result struct {
	Findings []Finding `json:"findings"`
}

// HasWarnings returns true if any finding is a warning.
func (r Result) HasWarnings() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityWarning {
			return true
		}
	}
	return false
}

// FormatText renders findings as human-readable output.
func (r Result) FormatText() string {
	if len(r.Findings) == 0 {
		return "No DB lint issues found.\n"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d DB lint issue(s):\n\n", len(r.Findings)))

	for _, f := range r.Findings {
		icon := "⚠️ "
		if f.Severity == SeverityInfo {
			icon = "ℹ️ "
		}
		if f.Field != "" {
			sb.WriteString(fmt.Sprintf("  %s[%s] %s.%s: %s\n", icon, f.Rule, f.Entity, f.Field, f.Message))
		} else {
			sb.WriteString(fmt.Sprintf("  %s[%s] %s: %s\n", icon, f.Rule, f.Entity, f.Message))
		}
	}

	return sb.String()
}

// LintProtoDir scans proto files in the given directory and returns lint findings.
func LintProtoDir(protoDBDir string) (Result, error) {
	entities, err := database.ParseProtoEntities(protoDBDir)
	if err != nil {
		return Result{}, fmt.Errorf("parsing proto entities: %w", err)
	}

	return LintEntities(entities), nil
}

// LintEntities runs all lint rules against parsed entities.
// This is the pure-logic function, testable without file I/O.
func LintEntities(entities []database.ProtoEntity) Result {
	var result Result

	for _, ent := range entities {
		result.Findings = append(result.Findings, checkMissingPK(ent)...)
		result.Findings = append(result.Findings, checkMissingFKAnnotation(ent)...)
		result.Findings = append(result.Findings, checkMissingFKIndex(ent)...)
		result.Findings = append(result.Findings, checkFlatStructure(ent)...)
		result.Findings = append(result.Findings, checkMissingTimestamps(ent)...)
		result.Findings = append(result.Findings, checkMissingSoftDelete(ent)...)
	}

	return result
}

// checkMissingPK warns when an entity has no field with primary_key and no field named "id".
func checkMissingPK(ent database.ProtoEntity) []Finding {
	for _, f := range ent.Fields {
		if f.IsPrimary {
			return nil
		}
		if f.Name == "id" {
			return nil
		}
	}
	return []Finding{{
		Rule:     "missing-pk",
		Severity: SeverityWarning,
		Entity:   ent.MessageName,
		Message:  "entity has no primary key field (no field with primary_key: true and no field named 'id')",
	}}
}

// checkMissingFKAnnotation warns when fields ending in _id lack a references annotation.
// Since the regex parser in database.ParseProtoEntities doesn't extract references,
// we check by convention: if a field ends in _id (other than just "id"), flag it.
func checkMissingFKAnnotation(ent database.ProtoEntity) []Finding {
	var findings []Finding
	for _, f := range ent.Fields {
		if strings.HasSuffix(f.Name, "_id") && f.Name != "id" {
			findings = append(findings, Finding{
				Rule:     "missing-fk-annotation",
				Severity: SeverityWarning,
				Entity:   ent.MessageName,
				Field:    f.Name,
				Message: fmt.Sprintf("field looks like a foreign key but has no explicit references annotation "+
					"(consider: references: \"%s.id\")", pluralize(strings.TrimSuffix(f.Name, "_id"))),
			})
		}
	}
	return findings
}

// checkMissingFKIndex warns when FK fields don't have an index for query performance.
func checkMissingFKIndex(ent database.ProtoEntity) []Finding {
	var findings []Finding
	for _, f := range ent.Fields {
		if strings.HasSuffix(f.Name, "_id") && f.Name != "id" {
			findings = append(findings, Finding{
				Rule:     "missing-fk-index",
				Severity: SeverityInfo,
				Entity:   ent.MessageName,
				Field:    f.Name,
				Message:  "foreign key field should have an index for query performance",
			})
		}
	}
	return findings
}

// checkFlatStructure detects groups of fields that look like they should be
// a separate entity (e.g., address_line1, address_line2, address_city).
func checkFlatStructure(ent database.ProtoEntity) []Finding {
	// Count fields by prefix (first segment before _).
	prefixCounts := make(map[string][]string)
	for _, f := range ent.Fields {
		parts := strings.SplitN(f.Name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		prefix := parts[0]
		// Skip common prefixes that aren't entity candidates.
		if prefix == "is" || prefix == "has" || prefix == "can" ||
			prefix == "created" || prefix == "updated" || prefix == "deleted" {
			continue
		}
		prefixCounts[prefix] = append(prefixCounts[prefix], f.Name)
	}

	var findings []Finding
	for prefix, fields := range prefixCounts {
		if len(fields) >= 3 {
			findings = append(findings, Finding{
				Rule:     "flat-structure",
				Severity: SeverityInfo,
				Entity:   ent.MessageName,
				Message: fmt.Sprintf("fields %s share prefix %q — consider extracting a separate %s entity",
					strings.Join(fields, ", "), prefix, capitalizeFirst(prefix)),
			})
		}
	}
	return findings
}

// checkMissingTimestamps warns when an entity lacks created_at/updated_at fields.
func checkMissingTimestamps(ent database.ProtoEntity) []Finding {
	hasCreatedAt := false
	hasUpdatedAt := false
	for _, f := range ent.Fields {
		switch f.Name {
		case "created_at":
			hasCreatedAt = true
		case "updated_at":
			hasUpdatedAt = true
		}
	}

	var findings []Finding
	if !hasCreatedAt || !hasUpdatedAt {
		missing := []string{}
		if !hasCreatedAt {
			missing = append(missing, "created_at")
		}
		if !hasUpdatedAt {
			missing = append(missing, "updated_at")
		}
		findings = append(findings, Finding{
			Rule:     "missing-timestamps",
			Severity: SeverityWarning,
			Entity:   ent.MessageName,
			Message:  fmt.Sprintf("entity is missing timestamp fields: %s (add timestamps: true to entity_options or add the fields)", strings.Join(missing, ", ")),
		})
	}
	return findings
}

// checkMissingSoftDelete warns when an entity lacks a deleted_at field.
func checkMissingSoftDelete(ent database.ProtoEntity) []Finding {
	for _, f := range ent.Fields {
		if f.Name == "deleted_at" {
			return nil
		}
	}
	return []Finding{{
		Rule:     "missing-soft-delete",
		Severity: SeverityInfo,
		Entity:   ent.MessageName,
		Message:  "entity has no deleted_at field — consider adding soft_delete: true to entity_options",
	}}
}

// pluralize applies simple English pluralization.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "y") {
		if len(s) >= 2 {
			prev := s[len(s)-2]
			if prev == 'a' || prev == 'e' || prev == 'i' || prev == 'o' || prev == 'u' {
				return s + "s"
			}
		}
		return s[:len(s)-1] + "ies"
	}
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") ||
		strings.HasSuffix(s, "sh") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	return s + "s"
}

// capitalizeFirst uppercases the first letter of a string.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}