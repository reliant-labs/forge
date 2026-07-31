package seeddata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/schemadef"
)

// Vocab is the optional, user-authored domain-vocabulary overlay for seed
// synthesis: per-column value pools keyed "table.column", resolved from
// db/seeds/vocab.yaml. The app author (usually an LLM, right after writing
// the migrations while domain context is fresh) teaches the seeder the
// project's vocabulary ONCE; matched columns then draw from these pools with
// the exact same deterministic column-local hash-pick the built-ins use, so
// every stability guarantee holds — same (schema, config, vocab) renders
// byte-identically, and adding vocab for one column never reshuffles another.
//
// The overlay supplies VOCABULARY only. Referential machinery (primary keys,
// foreign keys) stays the seeder's job and is
// never overridable; values are validated against the applied schema's
// constraints at ApplyVocab time so a bad value degrades to a warning, never
// a failed seed.
type Vocab struct {
	// Columns maps "table.column" to its resolved value pool (named-pool
	// references are flattened by LoadVocab).
	Columns map[string][]string
}

// vocabFile is the on-disk YAML shape:
//
//	pools:                       # shared named pools
//	  peptide_names: [BPC-157, Semaglutide]
//	columns:                     # "table.column" -> inline list or {pool: name}
//	  products.name: {pool: peptide_names}
//	  brands.name: [VitalPep, PepCore Labs]
type vocabFile struct {
	Pools   map[string][]string   `yaml:"pools"`
	Columns map[string]vocabEntry `yaml:"columns"`
}

// vocabEntry is one columns: value — an inline list of values, a
// {pool: name} reference to a shared named pool, or a {type: name} semantic
// type whose values gofakeit generates (see vocabtype.go).
type vocabEntry struct {
	values []string
	pool   string
	typ    string
}

// UnmarshalYAML accepts the two sanctioned entry shapes and rejects
// everything else with a line-numbered error (a malformed file must fail
// loudly, not silently seed generic data the author believes is overridden).
func (e *vocabEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		return node.Decode(&e.values)
	case yaml.MappingNode:
		var ref struct {
			Pool string `yaml:"pool"`
			Type string `yaml:"type"`
		}
		if err := node.Decode(&ref); err != nil {
			return err
		}
		switch {
		case ref.Pool != "" && ref.Type != "":
			return fmt.Errorf("line %d: a mapping entry sets pool or type, not both", node.Line)
		case ref.Pool != "":
			e.pool = ref.Pool
		case ref.Type != "":
			if !IsVocabType(ref.Type) {
				return fmt.Errorf("line %d: unknown type %q (supported: %s)",
					node.Line, ref.Type, strings.Join(VocabTypeNames(), ", "))
			}
			e.typ = ref.Type
		default:
			return fmt.Errorf("line %d: a mapping entry must be {pool: <name>} or {type: <name>}", node.Line)
		}
		return nil
	default:
		return fmt.Errorf("line %d: a column entry must be a value list, {pool: <name>} or {type: <name>}", node.Line)
	}
}

// VocabPath returns the conventional overlay location for a project's
// migrations directory — db/seeds/vocab.yaml, sibling of the db/seeds/custom
// SQL overlay (both derive from the configured migrations dir the same way).
func VocabPath(migDir string) string {
	return filepath.Join(filepath.Dir(migDir), "seeds", "vocab.yaml")
}

// LoadVocab reads and resolves the vocabulary overlay. A missing file returns
// (nil, nil) — exactly the built-in behavior. A malformed file (bad YAML, an
// entry that is neither a list nor {pool: name}, a reference to an undefined
// pool, an empty pool, a key that is not table.column) is a hard error naming
// the problem. A fully-commented scaffold parses as empty and is a no-op.
func LoadVocab(path string) (*Vocab, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read seed vocab %s: %w", path, err)
	}
	// Strict decoding: an unknown top-level key (`column:` for `columns:`)
	// must fail loudly, not silently seed generic data. A comment-only
	// scaffold decodes to io.EOF — an empty overlay.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f vocabFile
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse seed vocab %s: %w", path, err)
	}
	v := &Vocab{Columns: map[string][]string{}}
	for key, e := range f.Columns {
		if !strings.Contains(key, ".") {
			return nil, fmt.Errorf("seed vocab %s: column key %q must be table.column", path, key)
		}
		vals := e.values
		switch {
		case e.pool != "":
			pool, ok := f.Pools[e.pool]
			if !ok {
				return nil, fmt.Errorf("seed vocab %s: %s references undefined pool %q", path, key, e.pool)
			}
			vals = pool
		case e.typ != "":
			// A semantic type is expanded into a value pool HERE, at load, so
			// everything downstream sees one shape: a list of values, already
			// validated against the column's constraints by ApplyVocab like
			// any author-supplied list. Typed values get no special trust —
			// gofakeit does not know the column's length cap, and a generated
			// value that violates one must be skipped with a warning exactly
			// as a hand-written one is.
			table, column, _ := strings.Cut(key, ".")
			generated, err := VocabTypeValues(e.typ, 0, table, column, vocabTypePoolSize)
			if err != nil {
				return nil, fmt.Errorf("seed vocab %s: %s: %w", path, key, err)
			}
			vals = generated
		}
		if len(vals) == 0 {
			return nil, fmt.Errorf("seed vocab %s: %s has no values", path, key)
		}
		v.Columns[key] = vals
	}
	if len(v.Columns) == 0 {
		return nil, nil
	}
	return v, nil
}

// ApplyVocab attaches a vocabulary overlay to the plan: each valid
// (table.column, values) entry becomes the column's draw pool, taking
// precedence over built-in synthesis for that column. Values are validated
// against the column's introspected constraints — call it AFTER bounds are
// attached (BuildLivePlan does) so numeric range CHECKs are visible.
//
// Vocab problems never fail the seed: an invalid value is skipped, a column
// whose vocab is entirely invalid falls back to built-ins, and PK/FK
// columns are never overridable — each with a warning naming table.column and
// the constraint. Warnings are returned AND kept on the plan (VocabWarnings)
// so every consumer of a built plan can surface them. nil vocab is a no-op.
func (p *Plan) ApplyVocab(v *Vocab) []string {
	if v == nil || len(v.Columns) == 0 {
		return nil
	}
	keys := make([]string, 0, len(v.Columns))
	for k := range v.Columns {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic warning order

	var warns []string
	warnf := func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf("seed vocab: "+format, args...))
	}
	for _, key := range keys {
		table, column, _ := strings.Cut(key, ".")
		tp, cp, ok := p.colPlan(table, column)
		if !ok {
			warnf("%s: not a seedable column in the applied schema — ignored", key)
			continue
		}
		col := cp.col
		switch {
		case col.IsPK:
			warnf("%s: primary-key column — the seeder owns referential columns; ignored", key)
			continue
		case cp.fk != nil:
			warnf("%s: foreign-key column — the seeder owns referential columns; ignored", key)
			continue
		case p.managedRole(table, col) == managedDeletedAt:
			warnf("%s: managed soft-delete column (always seeded NULL) — ignored", key)
			continue
		}
		if col.IsArray || (col.Type != schemadef.TypeString && col.Type != schemadef.TypeInt &&
			col.Type != schemadef.TypeFloat && col.Type != schemadef.TypeJSON) {
			warnf("%s: column type %s does not take a vocabulary — ignored", key, col.DeclType)
			continue
		}

		pool, hasPool := p.pools.get(tp.table.Name, col.Name)
		bound, _ := p.bounds.get(tp.table.Name, col.Name)
		minLen, maxLen := LengthBounds(p.byName[table], col)
		var kept []string
		for _, val := range v.Columns[key] {
			if reason := vocabValueProblem(col, val, pool, hasPool, bound, minLen, maxLen); reason != "" {
				warnf("%s: value %q %s — skipped", key, val, reason)
				continue
			}
			kept = append(kept, val)
		}
		if len(kept) == 0 {
			warnf("%s: no valid values remain — using built-in synthesis", key)
			continue
		}
		if p.vocab == nil {
			p.vocab = map[string]map[string][]string{}
		}
		if p.vocab[table] == nil {
			p.vocab[table] = map[string][]string{}
		}
		p.vocab[table][column] = kept
	}
	p.vocabWarns = warns
	// The overlay changes which columns draw from a CLOSED pool, which changes
	// how a UNIQUE column can be satisfied — re-resolve the constraint pass.
	p.finalize()
	return warns
}

// VocabWarnings returns the warnings the last ApplyVocab produced. The seed
// itself never fails on vocab problems; the CLI surfaces these instead.
func (p *Plan) VocabWarnings() []string { return p.vocabWarns }

// uuidLiteralRE matches the canonical 8-4-4-4-12 hex UUID spelling — the only
// value shape a UUID-typed column accepts on INSERT.
var uuidLiteralRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// vocabValueProblem validates one vocab value against the column's type and
// introspected constraints. Returns "" when the value can seed, else a short
// human reason (the caller wraps it with table.column and "skipped"). The
// seed is transactional — one constraint-violating value would abort the
// whole seed — so anything not provably insertable is rejected here.
func vocabValueProblem(col schemadef.Column, val string, pool []string, hasPool bool, b NumBound, minLen, maxLen int) string {
	if hasPool {
		for _, allowed := range pool {
			if val == allowed {
				return ""
			}
		}
		return fmt.Sprintf("is not in the column's CHECK/enum vocabulary %v", pool)
	}
	switch col.Type {
	case schemadef.TypeJSON:
		// A jsonb/json column takes any parseable JSON document; postgres
		// rejects a non-JSON string at INSERT, so reject it here rather than
		// abort the transactional seed.
		if !json.Valid([]byte(val)) {
			return fmt.Sprintf("is not valid JSON (column type %s)", col.DeclType)
		}
	case schemadef.TypeInt:
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return fmt.Sprintf("is not an integer (column type %s)", col.DeclType)
		}
		if (b.Min != nil && n < *b.Min) || (b.Max != nil && n > *b.Max) {
			return fmt.Sprintf("is outside the CHECK range %s", b.describe())
		}
	case schemadef.TypeFloat:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Sprintf("is not a number (column type %s)", col.DeclType)
		}
		if (b.Min != nil && f < float64(*b.Min)) || (b.Max != nil && f > float64(*b.Max)) {
			return fmt.Sprintf("is outside the CHECK range %s", b.describe())
		}
	default: // string
		// A UUID-typed column rejects any non-UUID string at INSERT; other
		// exotic string-backed types (inet, macaddr, ...) are rare enough in
		// entity schemas that length/pool checks carry the validation load.
		if strings.EqualFold(col.DeclType, "UUID") && !uuidLiteralRE.MatchString(val) {
			return "is not a UUID (column type UUID)"
		}
		n := utf8.RuneCountInString(val)
		if maxLen > 0 && n > maxLen {
			return fmt.Sprintf("exceeds the %d-char cap (varchar/char_length CHECK)", maxLen)
		}
		if minLen > 0 && n < minLen {
			return fmt.Sprintf("is shorter than the %d-char minimum (char_length CHECK)", minLen)
		}
	}
	return ""
}

// describe renders a NumBound for warnings ("[1,5]", "[100,∞)").
func (b NumBound) describe() string {
	lo, hi := "-∞", "∞"
	if b.Min != nil {
		lo = strconv.FormatInt(*b.Min, 10)
	}
	if b.Max != nil {
		hi = strconv.FormatInt(*b.Max, 10)
	}
	return "[" + lo + "," + hi + "]"
}

// declLenRE captures the length cap of a varchar/char declaration
// ("character varying(20)", "VARCHAR(3)", "char(4)").
var declLenRE = regexp.MustCompile(`(?i)^(?:character varying|character|varchar|char|bpchar)\s*\(\s*(\d+)\s*\)`)

// LengthBounds merges char_length CHECK constraints and the declared
// varchar/char cap into (min, max) character lengths for a column. 0 means
// unbounded on that side; an exact `char_length(col) = N` sets both to N.
// Shared by vocab validation here and codegen's lifecycle-test fixtures.
func LengthBounds(t schemadef.Table, col schemadef.Column) (minLen, maxLen int) {
	// The introspected cap (information_schema.character_maximum_length).
	if col.MaxChars > 0 {
		maxLen = col.MaxChars
	}
	// A hand-built model may instead spell the cap inside DeclType.
	if m := declLenRE.FindStringSubmatch(strings.TrimSpace(col.DeclType)); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 && (maxLen == 0 || n < maxLen) {
			maxLen = n
		}
	}
	// pg_get_constraintdef spells these as e.g.
	// `CHECK ((char_length(code) = 3))` or, for varchar columns,
	// `CHECK ((char_length((code)::text) >= 2))`.
	lenRE := regexp.MustCompile(`(?:char_length|character_length|length)\(\(?` +
		regexp.QuoteMeta(col.Name) + `\)?(?:::[a-z_ ]+)?\)\s*(=|>=|<=|>|<)\s*(\d+)`)
	for _, ck := range t.Checks {
		if len(ck.Columns) != 1 || ck.Columns[0] != col.Name {
			continue
		}
		for _, m := range lenRE.FindAllStringSubmatch(ck.Def, -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			switch m[1] {
			case "=":
				minLen, maxLen = n, n
			case ">=":
				if n > minLen {
					minLen = n
				}
			case ">":
				if n+1 > minLen {
					minLen = n + 1
				}
			case "<=":
				if maxLen == 0 || n < maxLen {
					maxLen = n
				}
			case "<":
				if maxLen == 0 || n-1 < maxLen {
					maxLen = n - 1
				}
			}
		}
	}
	return minLen, maxLen
}
