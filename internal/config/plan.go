package config

// PlanFile represents a forge plan for batch scaffolding.
type PlanFile struct {
	ProjectName string         `yaml:"project_name"`
	GoModule    string         `yaml:"go_module"`
	GoVersion   string         `yaml:"go_version,omitempty"`
	License     string         `yaml:"license,omitempty"`
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
	TenantKey  bool   `yaml:"tenant_key,omitempty" json:"tenant_key,omitempty"`
}
