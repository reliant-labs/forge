package servicecatalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
)

// Decode validates bounded catalog bytes before a browser/hosted ingest consumes
// them. It authenticates no provenance: callers must verify the containing OCI
// bundle digest and apply their existing project/environment authorization.
func Decode(raw []byte) (Catalog, error) {
	var c Catalog
	if len(raw) > MaxBytes {
		return c, fmt.Errorf("service catalog exceeds %d bytes", MaxBytes)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("service catalog: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("service catalog: trailing data")
	}
	if c.Schema != Schema {
		return c, fmt.Errorf("unsupported service catalog schema %q", c.Schema)
	}
	source := func(s Source) bool {
		return s.Path == "" && s.Line == 0 || s.Line > 0 && !strings.ContainsAny(s.Path, "\\:\x00") && !strings.HasPrefix(s.Path, "/") && path.Clean(s.Path) == s.Path && s.Path != "." && s.Path != ".." && !strings.HasPrefix(s.Path, "../")
	}
	for _, p := range c.Processes {
		if !source(p.Source) {
			return c, fmt.Errorf("invalid process source")
		}
	}
	for _, v := range c.Components {
		if !source(v.Source) {
			return c, fmt.Errorf("invalid component source")
		}
		for _, m := range v.Methods {
			if !source(m.Source) {
				return c, fmt.Errorf("invalid method source")
			}
		}
		for _, impl := range v.ImplementationCandidates {
			if !source(impl.Source) {
				return c, fmt.Errorf("invalid implementation source")
			}
			for _, m := range impl.Methods {
				if !source(m.Source) {
					return c, fmt.Errorf("invalid implementation method source")
				}
			}
		}
	}
	for _, v := range c.Bindings {
		for _, m := range v.Methods {
			if !source(m.Source) {
				return c, fmt.Errorf("invalid interface source")
			}
		}
	}
	for _, v := range c.Clients {
		if !source(v.Source) {
			return c, fmt.Errorf("invalid client source")
		}
		if v.Resolution == "declared" && (!protoRef(v.Service) || !protoRef(v.ConfigField)) {
			return c, fmt.Errorf("invalid client metadata")
		}
	}
	for _, v := range c.Diagnostics {
		if !source(v.Source) {
			return c, fmt.Errorf("invalid diagnostic source")
		}
	}
	return c, nil
}

func protoRef(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i, r := range p {
			if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}
