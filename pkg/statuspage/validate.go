package statuspage

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrInvalid is wrapped by every error [Document.Validate] and [Decode]
// return for a document that breaks the schema.
var ErrInvalid = errors.New("invalid status page document")

var (
	idRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	accentRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate reports the first way d breaks forge.status/v1: an unknown
// schema, a missing or overlong field, a value outside its enum, a duplicate
// component or id, or an incident or window naming a component the page
// does not have.
func (d Document) Validate() error {
	if d.Schema != Schema {
		return invalid("schema %q (this reader knows %q)", d.Schema, Schema)
	}
	if d.GeneratedAt.IsZero() {
		return invalid("generated_at is not set")
	}
	if err := d.Page.Validate(); err != nil {
		return err
	}
	if len(d.Components) == 0 {
		return invalid("the page has no components")
	}
	if len(d.Components) > MaxComponents {
		return invalid("%d components, more than %d", len(d.Components), MaxComponents)
	}
	names := map[string]bool{}
	for _, c := range d.Components {
		if err := c.validate(); err != nil {
			return err
		}
		if names[c.Name] {
			return invalid("component %q is named twice", c.Name)
		}
		names[c.Name] = true
	}
	if len(d.Incidents) > MaxIncidents {
		return invalid("%d incidents, more than %d", len(d.Incidents), MaxIncidents)
	}
	ids := map[string]bool{}
	for _, in := range d.Incidents {
		if err := in.validate(names); err != nil {
			return err
		}
		if ids["incident/"+in.ID] {
			return invalid("incident id %q is used twice", in.ID)
		}
		ids["incident/"+in.ID] = true
	}
	if len(d.Maintenance) > MaxMaintenance {
		return invalid("%d maintenance windows, more than %d", len(d.Maintenance), MaxMaintenance)
	}
	for _, m := range d.Maintenance {
		if err := m.validate(names); err != nil {
			return err
		}
		if ids["maintenance/"+m.ID] {
			return invalid("maintenance id %q is used twice", m.ID)
		}
		ids["maintenance/"+m.ID] = true
	}
	return nil
}

// Validate reports the first way p breaks the schema.
func (p Page) Validate() error {
	if err := text("page title", p.Title, MaxTitle, true); err != nil {
		return err
	}
	if p.URL != "" {
		if err := ValidateURL(p.URL, false); err != nil {
			return invalid("page url: %v", err)
		}
	}
	if p.LogoURL != "" {
		if err := ValidateURL(p.LogoURL, true); err != nil {
			return invalid("logo_url: %v", err)
		}
	}
	if p.AccentColor != "" && !accentRE.MatchString(p.AccentColor) {
		return invalid("accent_color %q is not a #rrggbb colour", p.AccentColor)
	}
	if len(p.Links) > MaxLinks {
		return invalid("%d links, more than %d", len(p.Links), MaxLinks)
	}
	for _, l := range p.Links {
		if err := text("link label", l.Label, MaxLinkLabel, true); err != nil {
			return err
		}
		if err := ValidateURL(l.URL, false); err != nil {
			return invalid("link %q: %v", l.Label, err)
		}
	}
	return nil
}

// ValidateURL checks a URL a page shows or links to: absolute, http or
// https (https only when httpsOnly: an image an https page loads must be
// https too), with a host, no credential and at most [MaxURL] bytes.
func ValidateURL(raw string, httpsOnly bool) error {
	if len(raw) > MaxURL {
		return fmt.Errorf("url is longer than %d bytes", MaxURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q: %v", raw, err)
	}
	switch {
	case httpsOnly && u.Scheme != "https":
		return fmt.Errorf("url %q must be https", raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("url %q must be http or https", raw)
	case u.User != nil:
		return fmt.Errorf("url %q carries a credential", raw)
	case u.Host == "":
		return fmt.Errorf("url %q has no host", raw)
	}
	return nil
}

// ValidateComponentName checks a component's display name.
func ValidateComponentName(name string) error {
	return text("component name", name, MaxComponentName, true)
}

// ValidateDescription checks a component's description (empty is valid).
func ValidateDescription(desc string) error {
	return text("component description", desc, MaxDescription, false)
}

// text checks a single-line display string: valid UTF-8, no control
// characters, no surrounding space, at most max characters.
func text(what, s string, maxChars int, required bool) error {
	switch {
	case s == "" && required:
		return invalid("%s is empty", what)
	case !utf8.ValidString(s):
		return invalid("%s is not UTF-8", what)
	case utf8.RuneCountInString(s) > maxChars:
		return invalid("%s %q is longer than %d characters", what, s, maxChars)
	case strings.TrimSpace(s) != s:
		return invalid("%s %q has leading or trailing space", what, s)
	case strings.ContainsFunc(s, isControl):
		return invalid("%s %q contains a control character", what, s)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// body checks an update body: plain text, newlines allowed.
func body(what, s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return invalid("%s is empty", what)
	case !utf8.ValidString(s):
		return invalid("%s is not UTF-8", what)
	case len(s) > MaxUpdateBody:
		return invalid("%s is longer than %d bytes", what, MaxUpdateBody)
	case strings.ContainsFunc(s, func(r rune) bool { return isControl(r) && r != '\n' && r != '\t' && r != '\r' }):
		return invalid("%s contains a control character", what)
	}
	return nil
}

func id(what, s string) error {
	if len(s) > MaxID || !idRE.MatchString(s) {
		return invalid("%s id %q (letters, digits, '-' and '_', at most %d)", what, s, MaxID)
	}
	return nil
}

func (c Component) validate() error {
	if err := ValidateComponentName(c.Name); err != nil {
		return err
	}
	if err := ValidateDescription(c.Description); err != nil {
		return err
	}
	if !slices.Contains(States, c.State) {
		return invalid("component %q: state %q (expected one of %v)", c.Name, c.State, States)
	}
	if len(c.Days) > MaxDays {
		return invalid("component %q: %d days, more than %d", c.Name, len(c.Days), MaxDays)
	}
	prev := ""
	for _, day := range c.Days {
		t, err := time.Parse(time.DateOnly, day.Date)
		if err != nil || t.Format(time.DateOnly) != day.Date {
			return invalid("component %q: day %q is not a YYYY-MM-DD date", c.Name, day.Date)
		}
		if day.Date <= prev {
			return invalid("component %q: day %s is not after %s (days run oldest first, one each)", c.Name, day.Date, prev)
		}
		prev = day.Date
		if a := day.Availability; a != nil && (math.IsNaN(*a) || *a < 0 || *a > 1) {
			return invalid("component %q: day %s availability %v is outside 0..1", c.Name, day.Date, *a)
		}
	}
	return nil
}

func components(what string, named []string, have map[string]bool) error {
	seen := map[string]bool{}
	for _, n := range named {
		if !have[n] {
			return invalid("%s names component %q, which the page does not have", what, n)
		}
		if seen[n] {
			return invalid("%s names component %q twice", what, n)
		}
		seen[n] = true
	}
	return nil
}

func (in Incident) validate(have map[string]bool) error {
	if err := id("incident", in.ID); err != nil {
		return err
	}
	what := fmt.Sprintf("incident %q", in.ID)
	if err := text(what+" title", in.Title, MaxIncidentTitle, true); err != nil {
		return err
	}
	if !slices.Contains([]Impact{ImpactMinor, ImpactMajor, ImpactCritical}, in.Impact) {
		return invalid("%s: impact %q (expected minor, major or critical)", what, in.Impact)
	}
	if !validIncidentStatus(in.Status) {
		return invalid("%s: status %q (expected investigating, identified, monitoring or resolved)", what, in.Status)
	}
	if err := components(what, in.Components, have); err != nil {
		return err
	}
	if in.StartedAt.IsZero() {
		return invalid("%s: started_at is not set", what)
	}
	switch {
	case in.Status == IncidentResolved && in.ResolvedAt == nil:
		return invalid("%s is resolved but has no resolved_at", what)
	case in.Status != IncidentResolved && in.ResolvedAt != nil:
		return invalid("%s has resolved_at but its status is %s", what, in.Status)
	case in.ResolvedAt != nil && in.ResolvedAt.Before(in.StartedAt):
		return invalid("%s resolved before it started", what)
	}
	if len(in.Updates) == 0 {
		return invalid("%s has no published update", what)
	}
	if len(in.Updates) > MaxUpdates {
		return invalid("%s: %d updates, more than %d", what, len(in.Updates), MaxUpdates)
	}
	for i, u := range in.Updates {
		if !validIncidentStatus(u.Status) {
			return invalid("%s: update %d status %q", what, i, u.Status)
		}
		if err := body(fmt.Sprintf("%s update %d body", what, i), u.Body); err != nil {
			return err
		}
		if u.At.IsZero() {
			return invalid("%s: update %d has no time", what, i)
		}
	}
	return nil
}

func validIncidentStatus(s IncidentStatus) bool {
	return slices.Contains([]IncidentStatus{IncidentInvestigating, IncidentIdentified, IncidentMonitoring, IncidentResolved}, s)
}

func validMaintenanceStatus(s MaintenanceStatus) bool {
	return slices.Contains([]MaintenanceStatus{MaintenanceScheduled, MaintenanceInProgress, MaintenanceCompleted}, s)
}

func (m Maintenance) validate(have map[string]bool) error {
	if err := id("maintenance", m.ID); err != nil {
		return err
	}
	what := fmt.Sprintf("maintenance %q", m.ID)
	if err := text(what+" title", m.Title, MaxIncidentTitle, true); err != nil {
		return err
	}
	if !validMaintenanceStatus(m.Status) {
		return invalid("%s: status %q (expected scheduled, in_progress or completed)", what, m.Status)
	}
	if err := components(what, m.Components, have); err != nil {
		return err
	}
	if m.StartsAt.IsZero() || m.EndsAt.IsZero() {
		return invalid("%s needs starts_at and ends_at", what)
	}
	if !m.EndsAt.After(m.StartsAt) {
		return invalid("%s ends before it starts", what)
	}
	if len(m.Updates) == 0 {
		return invalid("%s has no published update", what)
	}
	if len(m.Updates) > MaxUpdates {
		return invalid("%s: %d updates, more than %d", what, len(m.Updates), MaxUpdates)
	}
	for i, u := range m.Updates {
		if !validMaintenanceStatus(u.Status) {
			return invalid("%s: update %d status %q", what, i, u.Status)
		}
		if err := body(fmt.Sprintf("%s update %d body", what, i), u.Body); err != nil {
			return err
		}
		if u.At.IsZero() {
			return invalid("%s: update %d has no time", what, i)
		}
	}
	return nil
}
