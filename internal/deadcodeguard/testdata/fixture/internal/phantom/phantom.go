// Package phantom plants one instance of every shape the phantom-field rule
// must judge, and one of every shape it must leave alone.
package phantom

import "sync"

// Component reproduces the real defect: config.ComponentConfig.Ports was read
// by PrimaryPort and by the components_gen emitter, and written by nothing but
// tests, so every consumer branched on a hard zero.
type Component struct {
	Name string
	// WANT phantom-field: read below, written only from the test file.
	Ports map[string]int
	// WANT phantom-field: read below, written nowhere at all.
	Schedule string
	// OK: written by NewComponent.
	Kind string
}

// PrimaryPort is the real shape of the defect — a lookup whose answer is
// pinned to zero because nothing ever fills the map it reads.
func (c Component) PrimaryPort() int {
	if c.Ports == nil {
		return 0
	}
	return c.Ports["http"]
}

// Describe reads Schedule, which nothing writes.
func (c Component) Describe() string {
	if c.Schedule != "" {
		return c.Name + " @ " + c.Schedule
	}
	return c.Name
}

// NewComponent writes Name and Kind and nothing else.
func NewComponent(name string) *Component {
	c := &Component{Name: name}
	c.Kind = "server"
	return c
}

// Tagged is a serialization target. Decoders fill Label by reflection, which
// no call graph can see, so no field of this struct may be judged.
type Tagged struct {
	Label string `json:"label"`
	Other int    `json:"other"`
}

// Label is read and never written in Go — the json decoder is the writer.
func (t Tagged) Describe() string { return t.Label }

// Seams holds the shapes that look phantom but are not.
type Seams struct {
	// OK: a func-typed field is an injection seam; nil means "default".
	Runner func() error
	// OK: an interface-typed field is the same.
	Sink interface{ Write([]byte) (int, error) }
	// OK: a mutex is used, never assigned — Lock() has a pointer receiver.
	mu sync.Mutex
	// OK: written positionally by NewSeams' unkeyed literal.
	Positional string
	guarded    int
}

// Use exercises the seam fields so they are read at least once.
func (s *Seams) Use() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Runner != nil {
		_ = s.Runner()
	}
	if s.Sink != nil {
		_, _ = s.Sink.Write(nil)
	}
	return s.Positional, s.guarded
}

// NewSeams writes Positional and guarded through an UNKEYED literal, which the
// rule must count as a write exactly like a keyed one.
func NewSeams() *Seams {
	return &Seams{nil, nil, sync.Mutex{}, "positional", 7}
}

// ── Look-alike: written only THROUGH a subfield ──────────────────────────────
//
// This is the real ProjectGenerator.Features shape, and it must NOT be
// reported. Nothing assigns Flags wholesale; production mutates it by writing
// one of its subfields. Assigning through a chain mutates every struct along
// that chain, so `g.Flags.Verbose = &b` is a WRITE of Flags, not a read of it.
//
// Getting this wrong reported a field that dozens of production sites read and
// that production really does write, which would have led to deleting working
// code — and a rule that cries wolf gets weakened until it protects nothing.
type Flags struct {
	Verbose *bool
	Quiet   *bool
}

type Runner struct {
	Name string
	// OK: never assigned wholesale, but written through in applyDefaults below.
	Flags Flags
}

// applyDefaults is the production writer — through the field, not to it.
func (r *Runner) applyDefaults() {
	off := func() *bool { b := false; return &b }
	if r.Flags.Verbose == nil {
		r.Flags.Verbose = off()
	}
	if r.Flags.Quiet == nil {
		r.Flags.Quiet = off()
	}
}

// Loud reads Flags, the same way production reads it in dozens of places.
func (r Runner) Loud() bool {
	r2 := r
	r2.applyDefaults()
	return r2.Flags.Verbose != nil && *r2.Flags.Verbose
}
