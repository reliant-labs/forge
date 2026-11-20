// Package cli — the old command spellings forge still accepts.
//
// v1.9.0 removed the old top-level spellings of the commands that moved under
// a noun (`forge skill` is `forge agent skill`; the migrations/v1.9.0 playbook
// maps each). The rows below are the exceptions: hint strings in files an
// open pull request owns still print them, so they stay mounted — hidden from
// --help, silent, and built by the same constructor as their new path — until
// that pull request lands and moves its strings.
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
)

// keptAlias is one old spelling still mounted at Old, running the command at
// New.
type keptAlias struct {
	Old, New string
	// build constructs the command mounted at Old. Its Name() must be Old's
	// last word.
	build func(*aliasBuilder) *cobra.Command
}

// aliasBuilder is what a row's constructor may need: the factory the
// dir-nested command groups (internal/cli/<group>) are built from, and those
// groups' constructors by name.
type aliasBuilder struct {
	f      *factory.Factory
	groups map[string]factory.CmdFactory
}

func (b *aliasBuilder) group(name string) *cobra.Command {
	makeCmd, ok := b.groups[name]
	if !ok {
		panic(fmt.Sprintf("keptAliases names the command group %q, which no internal/cli/<group> registers", name))
	}
	return makeCmd(b.f)
}

// keptAliases is the table.
var keptAliases = []keptAlias{
	// kept until forge#2385 lands: generate_pipeline.go still prints this spelling.
	{Old: "doctor", New: "project doctor", build: func(*aliasBuilder) *cobra.Command { return newDoctorCmd() }},
	// kept until forge#2385 lands: generate_pipeline.go still prints this spelling.
	{Old: "cluster up", New: "dev cluster up", build: func(*aliasBuilder) *cobra.Command { return newDevClusterUpCmd() }},
	// kept until forge#2385 lands: generate_pipeline.go and generate_middleware.go still print this spelling.
	{Old: "fix", New: "lint fix", build: func(b *aliasBuilder) *cobra.Command { return b.group("fix") }},
	// kept until forge#2385 lands: scaffold/scaffold.go and generator/project.go still print this spelling.
	{Old: "package new", New: "scaffold package", build: func(b *aliasBuilder) *cobra.Command {
		scaffold := b.group("scaffold")
		c := findChildCommand(scaffold, "package")
		scaffold.RemoveCommand(c)
		c.Use = "new" + strings.TrimPrefix(c.Use, "package")
		return c
	}},
	// kept until forge#2385 lands: tools.go, generate_buf.go, generate_toolpins.go and new.go still print this spelling.
	{Old: "tools", New: "machine tools", build: func(*aliasBuilder) *cobra.Command { return newToolsCmd() }},
	// kept until forge#2385 lands: scaffold/scaffold.go, codegen/crud_gen.go and new.go still print this spelling.
	{Old: "skill", New: "agent skill", build: func(*aliasBuilder) *cobra.Command { return newSkillCmd() }},
}

// mountKeptAliases mounts every row at its Old path, hidden. A parent the
// path needs that the tree lacks (`cluster` for `cluster up`) is created as a
// hidden group. It panics on a table that does not fit the tree, which every
// test run catches.
func mountKeptAliases(root *cobra.Command, b *aliasBuilder) {
	for _, a := range keptAliases {
		words := strings.Fields(a.Old)
		parent := root
		for _, w := range words[:len(words)-1] {
			next := findChildCommand(parent, w)
			if next == nil {
				next = cmdutil.StrictGroup(&cobra.Command{Use: w, Hidden: true, Short: "Old spelling: see 'forge " + strings.Fields(a.New)[0] + "'"})
				parent.AddCommand(next)
			}
			parent = next
		}
		name := words[len(words)-1]
		if findChildCommand(parent, name) != nil {
			panic(fmt.Sprintf("keptAliases mounts %q, which already exists", a.Old))
		}
		c := a.build(b)
		if c.Name() != name {
			panic(fmt.Sprintf("keptAliases row %q builds a command named %q", a.Old, c.Name()))
		}
		c.Hidden = true
		parent.AddCommand(c)
	}
}

// canonicalRel maps a path under a kept old spelling to its new one ("tools
// install" → "machine tools install"), so every path-keyed table is written
// once, in the new spelling. Any other path is returned as is.
func canonicalRel(rel string) string {
	for _, a := range keptAliases {
		if rel == a.Old || strings.HasPrefix(rel, a.Old+" ") {
			return a.New + rel[len(a.Old):]
		}
	}
	return rel
}
