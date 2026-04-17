package config

// PlanFile represents a forge plan for batch scaffolding.
type PlanFile struct {
	ProjectName string        `yaml:"project_name"`
	GoModule    string        `yaml:"go_module"`
	GoVersion   string        `yaml:"go_version,omitempty"`
	License     string        `yaml:"license,omitempty"`
	Services    []PlanService `yaml:"services,omitempty"`
	Packages    []PlanPackage `yaml:"packages,omitempty"`
	Frontends   []PlanFrontend `yaml:"frontends,omitempty"`
}

// PlanService describes a service to scaffold.
type PlanService struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
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
}
