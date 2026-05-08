package generator

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// EntityDefsToSeedEntities converts codegen.EntityDef slices (parsed from
// proto/db/) into SeedEntity slices suitable for seed-data generation.
func EntityDefsToSeedEntities(defs []codegen.EntityDef) []SeedEntity {
	entities := make([]SeedEntity, 0, len(defs))
	for _, d := range defs {
		ent := SeedEntity{
			TableName:  d.TableName,
			Timestamps: hasTimestampFields(d),
			SoftDelete: hasSoftDelete(d),
		}
		for _, f := range d.Fields {
			sf := SeedField{
				ColumnName: f.Name,
				FieldType:  goTypeToSeedFieldType(f.GoType),
				IsPK:       f.Name == d.PkField,
				NotNull:    true,
				IsTimestamp: isTimestampColumn(f.Name),
			}
			if f.IsFK {
				sf.References = f.FKTable + ".id"
			}
			ent.Fields = append(ent.Fields, sf)
		}
		entities = append(entities, ent)
	}
	return entities
}

func hasTimestampFields(d codegen.EntityDef) bool {
	for _, f := range d.Fields {
		if f.Name == "created_at" || f.Name == "updated_at" {
			return true
		}
	}
	return false
}

func hasSoftDelete(d codegen.EntityDef) bool {
	for _, f := range d.Fields {
		if f.Name == "deleted_at" {
			return true
		}
	}
	return false
}

func isTimestampColumn(name string) bool {
	return name == "created_at" || name == "updated_at" || name == "deleted_at" ||
		strings.HasSuffix(name, "_at")
}

func goTypeToSeedFieldType(goType string) SeedFieldType {
	switch goType {
	case "string":
		return SeedFieldText
	case "int32", "int":
		return SeedFieldInteger
	case "int64":
		return SeedFieldBigInt
	case "uint32", "uint64":
		return SeedFieldBigInt
	case "float32", "float64":
		return SeedFieldFloat
	case "bool":
		return SeedFieldBoolean
	case "[]byte":
		return SeedFieldBytes
	case "timestamppb.Timestamp":
		return SeedFieldTimestamp
	default:
		return SeedFieldText
	}
}

// defaultSeedCount is the number of records generated per entity.
const defaultSeedCount = 10

// SeedFieldType represents the SQL column type for seed generation.
type SeedFieldType int

const (
	SeedFieldText SeedFieldType = iota
	SeedFieldUUID
	SeedFieldInteger
	SeedFieldBigInt
	SeedFieldBoolean
	SeedFieldTimestamp
	SeedFieldFloat
	SeedFieldBytes
)

// SeedField describes a single column for seed-data generation.
type SeedField struct {
	ColumnName    string
	FieldType     SeedFieldType
	IsPK          bool
	NotNull       bool
	IsTimestamp   bool
	AutoIncrement bool
	References    string // FK reference in "table.column" format
}

// SeedEntity describes a database entity for seed-data generation.
type SeedEntity struct {
	TableName  string
	Fields     []SeedField
	SoftDelete bool
	Timestamps bool
}

// seedNamespace is a fixed UUID namespace for deterministic UUID generation.
// This is an arbitrary UUID v4 chosen once — changing it changes all generated UUIDs.
var seedNamespace = [16]byte{
	0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

// deterministicUUID generates a UUID v5-style deterministic UUID from a name string.
// The output is stable: the same name always produces the same UUID.
func deterministicUUID(name string) string {
	h := sha1.New()
	h.Write(seedNamespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	// Set version 5 (bits 4-7 of byte 6)
	sum[6] = (sum[6] & 0x0f) | 0x50
	// Set variant (bits 6-7 of byte 8)
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// GenerateEntitySeeds generates SQL seed files and JSON fixture files for the
// given entities. SQL files go to <outputDir>/db/seeds/0002_<table>.sql and
// JSON files go to <outputDir>/db/fixtures/<table>.json.
func GenerateEntitySeeds(entities []SeedEntity, outputDir string) error {
	return generateEntitySeeds(entities, outputDir)
}

func generateEntitySeeds(entities []SeedEntity, outputDir string) error {
	if len(entities) == 0 {
		return nil
	}

	seedsDir := filepath.Join(outputDir, "db", "seeds")
	fixturesDir := filepath.Join(outputDir, "db", "fixtures")
	if err := os.MkdirAll(seedsDir, 0o755); err != nil {
		return fmt.Errorf("create seeds dir: %w", err)
	}
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return fmt.Errorf("create fixtures dir: %w", err)
	}

	for i, ent := range entities {
		records := generateRecords(ent, defaultSeedCount)

		// SQL seed file — numbered starting at 0002 (0001 is the static items seed).
		sqlContent := renderSQLSeed(ent, records)
		sqlFile := filepath.Join(seedsDir, fmt.Sprintf("%04d_%s.sql", i+2, ent.TableName))
		if err := os.WriteFile(sqlFile, []byte(sqlContent), 0o644); err != nil {
			return fmt.Errorf("write seed %s: %w", sqlFile, err)
		}

		// JSON fixture file.
		jsonContent, err := renderJSONFixture(ent, records)
		if err != nil {
			return fmt.Errorf("marshal fixture %s: %w", ent.TableName, err)
		}
		jsonFile := filepath.Join(fixturesDir, ent.TableName+".json")
		if err := os.WriteFile(jsonFile, jsonContent, 0o644); err != nil {
			return fmt.Errorf("write fixture %s: %w", jsonFile, err)
		}
	}
	return nil
}

// record is an ordered list of column→value pairs (preserves insertion order).
type record struct {
	columns []string
	values  []string
}

// generateRecords builds `count` seed records for the given entity.
func generateRecords(ent SeedEntity, count int) []record {
	// Collect fields to seed — skip deleted_at for soft-delete entities and
	// skip auto-increment PKs (the DB generates those).
	var fields []SeedField
	for _, f := range ent.Fields {
		if f.ColumnName == "deleted_at" {
			continue
		}
		if f.AutoIncrement {
			continue
		}
		fields = append(fields, f)
	}

	records := make([]record, count)
	for i := 0; i < count; i++ {
		r := record{}
		for _, f := range fields {
			r.columns = append(r.columns, f.ColumnName)
			r.values = append(r.values, generateValue(ent.TableName, f, i))
		}
		records[i] = r
	}
	return records
}

// generateValue produces a single deterministic cell value for the given field and row index.
func generateValue(tableName string, f SeedField, i int) string {
	col := f.ColumnName

	// Primary key UUID
	if f.IsPK && f.FieldType == SeedFieldUUID {
		return deterministicUUID(fmt.Sprintf("%s.%d", tableName, i))
	}
	if f.IsPK && f.FieldType == SeedFieldText && col == "id" {
		return deterministicUUID(fmt.Sprintf("%s.%d", tableName, i))
	}

	// Foreign key references — generate a deterministic UUID for the referenced table.
	if f.References != "" && (f.FieldType == SeedFieldText || f.FieldType == SeedFieldUUID) {
		parts := strings.SplitN(f.References, ".", 2)
		refTable := parts[0]
		// Reference the i-th record in the target table (mod defaultSeedCount).
		return deterministicUUID(fmt.Sprintf("%s.%d", refTable, i%defaultSeedCount))
	}

	// Timestamp fields
	if f.IsTimestamp || f.FieldType == SeedFieldTimestamp {
		return generateTimestamp(col, i)
	}

	// Boolean
	if f.FieldType == SeedFieldBoolean {
		if i%2 == 0 {
			return "true"
		}
		return "false"
	}

	// Integer / BigInt
	if f.FieldType == SeedFieldInteger || f.FieldType == SeedFieldBigInt {
		return generateIntegerValue(col, i)
	}

	// Float
	if f.FieldType == SeedFieldFloat {
		return fmt.Sprintf("%.2f", float64(i+1)*10.5)
	}

	// String fields — match by column name pattern.
	return generateStringValue(col, i)
}

// generateTimestamp returns a deterministic timestamp string for the given column and index.
func generateTimestamp(col string, i int) string {
	// Spread across January 2024, one day apart.
	day := (i % 28) + 1
	switch col {
	case "updated_at":
		return fmt.Sprintf("2024-01-%.2dT12:00:00Z", day)
	default: // created_at, and any other timestamp
		return fmt.Sprintf("2024-01-%.2dT08:00:00Z", day)
	}
}

// generateIntegerValue returns a deterministic integer string for the given column and index.
func generateIntegerValue(col string, i int) string {
	switch {
	case col == "age":
		return fmt.Sprintf("%d", 20+(i%50))
	case col == "quantity" || col == "count":
		return fmt.Sprintf("%d", (i+1)*5)
	case col == "price" || col == "amount" || strings.HasSuffix(col, "_cents"):
		return fmt.Sprintf("%d", (i+1)*1000)
	case col == "sort_order" || col == "position" || col == "priority":
		return fmt.Sprintf("%d", i+1)
	default:
		return fmt.Sprintf("%d", i+1)
	}
}

// generateStringValue returns a deterministic string for the given column name and index.
func generateStringValue(col string, i int) string {
	switch {
	case col == "name":
		return sampleNames[i%len(sampleNames)]
	case col == "first_name":
		return sampleFirstNames[i%len(sampleFirstNames)]
	case col == "last_name":
		return sampleLastNames[i%len(sampleLastNames)]
	case col == "email":
		return fmt.Sprintf("user%d@example.com", i+1)
	case col == "phone" || col == "phone_number":
		return fmt.Sprintf("+1555%07d", i+1)
	case col == "title":
		return sampleTitles[i%len(sampleTitles)]
	case col == "description":
		return sampleDescriptions[i%len(sampleDescriptions)]
	case col == "url" || col == "website" || col == "homepage":
		return fmt.Sprintf("https://example.com/%s/%d", col, i+1)
	case col == "avatar_url" || col == "image_url" || col == "photo_url":
		return fmt.Sprintf("https://example.com/avatars/%d.png", i+1)
	case col == "address" || col == "street" || col == "address_line_1":
		return sampleAddresses[i%len(sampleAddresses)]
	case col == "city":
		return sampleCities[i%len(sampleCities)]
	case col == "state":
		return sampleStates[i%len(sampleStates)]
	case col == "country":
		return sampleCountries[i%len(sampleCountries)]
	case col == "zip" || col == "zip_code" || col == "postal_code":
		return fmt.Sprintf("%05d", 10001+i)
	case col == "status":
		return sampleStatuses[i%len(sampleStatuses)]
	case col == "role":
		return sampleRoles[i%len(sampleRoles)]
	case col == "type" || col == "kind" || col == "category":
		return sampleTypes[i%len(sampleTypes)]
	case col == "slug":
		return fmt.Sprintf("item-%d", i+1)
	case col == "username":
		return fmt.Sprintf("user_%d", i+1)
	case col == "password_hash" || col == "password_digest":
		return fmt.Sprintf("$2a$10$fakehash%040d", i)
	case col == "token" || col == "api_key" || col == "secret":
		return fmt.Sprintf("tok_%032d", i+1)
	case col == "code":
		return fmt.Sprintf("CODE-%04d", i+1)
	case col == "notes" || col == "comment" || col == "bio":
		return sampleDescriptions[i%len(sampleDescriptions)]
	case col == "currency":
		return sampleCurrencies[i%len(sampleCurrencies)]
	case col == "locale" || col == "language":
		return sampleLocales[i%len(sampleLocales)]
	case col == "timezone":
		return sampleTimezones[i%len(sampleTimezones)]
	case strings.HasSuffix(col, "_id"):
		// Untyped FK-like string — generate a deterministic UUID.
		refTable := strings.TrimSuffix(col, "_id") + "s"
		return deterministicUUID(fmt.Sprintf("%s.%d", refTable, i%defaultSeedCount))
	case col == "id":
		// Non-PK id field (unlikely but handle it)
		return deterministicUUID(fmt.Sprintf("misc.%d", i))
	default:
		return fmt.Sprintf("sample_%s_%d", col, i+1)
	}
}

// renderSQLSeed renders a complete SQL seed file for one entity.
func renderSQLSeed(ent SeedEntity, records []record) string {
	if len(records) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("-- Auto-generated seed data for %s\n", ent.TableName))
	b.WriteString(fmt.Sprintf("INSERT INTO %s (", ent.TableName))
	b.WriteString(strings.Join(records[0].columns, ", "))
	b.WriteString(") VALUES\n")

	for i, r := range records {
		b.WriteString("    (")
		for j, v := range r.values {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString(sqlQuote(ent.Fields, r.columns[j], v))
		}
		b.WriteString(")")
		if i < len(records)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}

	// Use the PK column for ON CONFLICT (fall back to "id").
	conflictCol := "id"
	for _, f := range ent.Fields {
		if f.IsPK {
			conflictCol = f.ColumnName
			break
		}
	}
	b.WriteString(fmt.Sprintf("ON CONFLICT (%s) DO NOTHING;\n", conflictCol))
	return b.String()
}

// sqlQuote wraps a value in appropriate SQL quoting based on its field type.
func sqlQuote(fields []SeedField, colName, value string) string {
	for _, f := range fields {
		if f.ColumnName == colName {
			switch f.FieldType {
			case SeedFieldBoolean:
				return value // true/false are bare keywords in Postgres
			case SeedFieldInteger, SeedFieldBigInt, SeedFieldFloat:
				return value
			default:
				return "'" + strings.ReplaceAll(value, "'", "''") + "'"
			}
		}
	}
	// Default: quote as string.
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// renderJSONFixture renders a JSON fixture file matching the database.SeedData format.
func renderJSONFixture(ent SeedEntity, records []record) ([]byte, error) {
	rows := make([]map[string]interface{}, len(records))
	for i, r := range records {
		row := make(map[string]interface{}, len(r.columns))
		for j, col := range r.columns {
			row[col] = typedJSONValue(ent.Fields, col, r.values[j])
		}
		rows[i] = row
	}

	fixture := map[string]interface{}{
		"name":        ent.TableName,
		"description": "Auto-generated seed data",
		"tables": map[string]interface{}{
			ent.TableName: rows,
		},
	}

	return json.MarshalIndent(fixture, "", "  ")
}

// typedJSONValue converts a string value to a typed JSON value based on the field type.
func typedJSONValue(fields []SeedField, colName, value string) interface{} {
	for _, f := range fields {
		if f.ColumnName == colName {
			switch f.FieldType {
			case SeedFieldBoolean:
				return value == "true"
			case SeedFieldInteger:
				var n int
				fmt.Sscanf(value, "%d", &n)
				return n
			case SeedFieldBigInt:
				var n int64
				fmt.Sscanf(value, "%d", &n)
				return n
			case SeedFieldFloat:
				var f64 float64
				fmt.Sscanf(value, "%f", &f64)
				return f64
			}
		}
	}
	return value
}

// ──────────────────────────────────────────────────────────────────────
// Sample data arrays — professional/business-oriented for SaaS apps.
// ──────────────────────────────────────────────────────────────────────

var sampleNames = []string{
	"Acme Corp", "Globex Industries", "Initech Solutions",
	"Umbrella Holdings", "Soylent Inc", "Stark Enterprises",
	"Wayne Industries", "Oscorp Technologies", "Hooli Systems",
	"Pied Piper", "Dunder Mifflin", "Sterling Cooper",
	"Tyrell Corporation", "Weyland-Yutani", "Cyberdyne Systems",
	"Massive Dynamic", "InGen Labs", "Aperture Science",
	"Wonka Industries", "Nakatomi Trading",
}

var sampleFirstNames = []string{
	"Alice", "Bob", "Charlie", "Diana", "Edward",
	"Fiona", "George", "Hannah", "Ivan", "Julia",
	"Kevin", "Laura", "Michael", "Nora", "Oscar",
	"Patricia", "Quinn", "Rachel", "Samuel", "Teresa",
}

var sampleLastNames = []string{
	"Anderson", "Baker", "Chen", "Davis", "Evans",
	"Foster", "Garcia", "Harris", "Ibrahim", "Johnson",
	"Kim", "Lee", "Martinez", "Nguyen", "O'Brien",
	"Patel", "Quinn", "Robinson", "Singh", "Taylor",
}

var sampleTitles = []string{
	"Getting Started Guide", "API Integration Manual",
	"Security Best Practices", "Performance Tuning",
	"Architecture Overview", "Deployment Playbook",
	"Data Migration Handbook", "Monitoring Setup",
	"Incident Response Plan", "Onboarding Checklist",
}

var sampleDescriptions = []string{
	"Comprehensive guide for new team members and initial setup.",
	"Step-by-step instructions for integrating with external APIs.",
	"Best practices for securing production environments.",
	"Techniques for optimizing database queries and response times.",
	"High-level overview of the system architecture and design decisions.",
	"Detailed procedures for deploying services to production.",
	"Instructions for migrating data between schema versions.",
	"How to set up monitoring, alerting, and dashboards.",
	"Procedures for identifying, triaging, and resolving incidents.",
	"Checklist for onboarding new services and dependencies.",
}

var sampleAddresses = []string{
	"100 Main Street", "200 Oak Avenue", "350 Pine Boulevard",
	"42 Innovation Drive", "1600 Amphitheatre Parkway",
	"1 Infinite Loop", "500 Technology Square",
	"750 Market Street", "900 North Michigan Avenue",
	"1200 Fifth Avenue",
}

var sampleCities = []string{
	"San Francisco", "New York", "Austin", "Seattle", "Chicago",
	"Boston", "Denver", "Portland", "Atlanta", "Miami",
}

var sampleStates = []string{
	"CA", "NY", "TX", "WA", "IL",
	"MA", "CO", "OR", "GA", "FL",
}

var sampleCountries = []string{
	"US", "CA", "GB", "DE", "FR",
	"JP", "AU", "BR", "IN", "KR",
}

var sampleStatuses = []string{
	"active", "pending", "inactive", "archived", "suspended",
}

var sampleRoles = []string{
	"admin", "member", "viewer", "editor", "owner",
}

var sampleTypes = []string{
	"standard", "premium", "enterprise", "trial", "free",
}

var sampleCurrencies = []string{
	"USD", "EUR", "GBP", "JPY", "CAD",
}

var sampleLocales = []string{
	"en-US", "en-GB", "de-DE", "fr-FR", "ja-JP",
}

var sampleTimezones = []string{
	"America/New_York", "America/Chicago", "America/Denver",
	"America/Los_Angeles", "Europe/London",
	"Europe/Berlin", "Asia/Tokyo", "Asia/Shanghai",
	"Australia/Sydney", "America/Sao_Paulo",
}