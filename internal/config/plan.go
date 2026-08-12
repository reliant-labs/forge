package config

// PlanFile represents a forge plan for batch scaffolding.
type PlanFile struct {
	ProjectName string         `yaml:"project_name"`
	GoModule    string         `yaml:"go_module"`
	GoVersion   string         `yaml:"go_version,omitempty"`
	MockData    bool           `yaml:"mock_data,omitempty"`
	Services    []PlanService  `yaml:"services,omitempty"`
	Packages    []PlanPackage  `yaml:"packages,omitempty"`
	Frontends   []PlanFrontend `yaml:"frontends,omitempty"`
	Entities    []PlanEntity   `yaml:"entities,omitempty" json:"entities,omitempty"`
}

// PlanService describes a service to scaffold.
type PlanService struct {
	Name        string    `yaml:"name" json:"name"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	RPCs        []PlanRPC `yaml:"rpcs,omitempty" json:"rpcs,omitempty"`
}

// PlanRPC describes an RPC to scaffold in a service proto.
type PlanRPC struct {
	Name        string      `yaml:"name" json:"name"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
	Request     []PlanField `yaml:"request,omitempty" json:"request,omitempty"`
	Response    []PlanField `yaml:"response,omitempty" json:"response,omitempty"`
}

// PlanField describes a field in a proto message.
type PlanField struct {
	Name string `yaml:"name" json:"name"`
	Type string `yaml:"type" json:"type"`
}

// PlanPackage describes an internal package to scaffold.
type PlanPackage struct {
	Name        string `yaml:"name"`
	Kind        string `yaml:"kind,omitempty"` // "eventbus", "client", or empty
	Description string `yaml:"description,omitempty"`
}

// PlanFrontend describes a frontend to scaffold.
type PlanFrontend struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind,omitempty"` // "mobile" for React Native; empty/default = Next.js web
}

// PlanEntity describes a database entity to scaffold.
type PlanEntity struct {
	Name       string            `yaml:"name" json:"name"`                                 // PascalCase message name, e.g. "Project"
	TableName  string            `yaml:"table_name,omitempty" json:"table_name,omitempty"` // override; defaults to pluralized snake_case
	SoftDelete bool              `yaml:"soft_delete,omitempty" json:"soft_delete,omitempty"`
	Timestamps bool              `yaml:"timestamps,omitempty" json:"timestamps,omitempty"`
	Fields     []PlanEntityField `yaml:"fields" json:"fields"`
}

// PlanEntityField describes a field on an entity.
type PlanEntityField struct {
	Name       string `yaml:"name" json:"name"` // snake_case proto field name
	Type       string `yaml:"type" json:"type"` // "string", "int64", "bool", "google.protobuf.Timestamp"
	PrimaryKey bool   `yaml:"primary_key,omitempty" json:"primary_key,omitempty"`
	NotNull    bool   `yaml:"not_null,omitempty" json:"not_null,omitempty"`
	Unique     bool   `yaml:"unique,omitempty" json:"unique,omitempty"`
	Default    string `yaml:"default,omitempty" json:"default,omitempty"`
	References string `yaml:"references,omitempty" json:"references,omitempty"` // "users.id"
	// Generated marks a GENERATED ALWAYS AS (...) STORED column: the DB
	// computes it, so the ORM emits ,scanonly (never written on
	// INSERT/UPDATE).
	Generated bool `yaml:"generated,omitempty" json:"generated,omitempty"`
	// Secret marks a column backed by a `// forge:secret` wire field: the
	// read path never packs it (a client always reads it back as ""), so the
	// generated repo's Spec.SecretColumns preserves it on a maskless
	// full-replace Update rather than clobbering the stored value with "".
	Secret bool `yaml:"secret,omitempty" json:"secret,omitempty"`
	// Immutable marks a column declared `forge:immutable` in a COMMENT ON
	// COLUMN. It projects to Bun's `skipupdate` tag option, so a full-replace
	// UPDATE omits it from the SET clause while an explicit update_mask
	// naming it still writes it.
	//
	// The declaration lives in the MIGRATION, not the proto: whether a column
	// may be rewritten is a fact about storage, and the wire contract and the
	// schema evolve on independent clocks. Inferring it from the absence of a
	// wire field is wrong — a column can be absent from the API and still be
	// ordinary mutable state (an internal score, a denormalized cache).
	Immutable bool `yaml:"immutable,omitempty" json:"immutable,omitempty"`
	// Version marks a column declared `forge:version` in a COMMENT ON
	// COLUMN. It opts the entity into optimistic concurrency control:
	// forge/pkg/crud's Repo adds it to Update/UpdateMasked's WHERE clause
	// (matched against the caller's last-read value) and increments it on
	// a successful write, failing a lost race with svcerr.ErrAborted
	// rather than silently overwriting a concurrent writer's change.
	Version bool `yaml:"version,omitempty" json:"version,omitempty"`
	// FillStrategy carries a `forge:fill=<strategy>` COMMENT ON COLUMN
	// declaration verbatim ("ulid" or "handler"), "" for none. "ulid" makes
	// forge/pkg/crud's Repo generate one at Create for this (non-PK) column,
	// the same chokepoint that already ULID-generates an empty string PK.
	// "handler" changes no codegen behavior — it only suppresses the
	// unsatisfiable-column lint. See schemadef.ColumnMarkerFill.
	FillStrategy string `yaml:"fill_strategy,omitempty" json:"fill_strategy,omitempty"`
}
