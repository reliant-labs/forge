package codegen

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/mergeproperty"
	"github.com/reliant-labs/forge/pkg/dbgrants"
)

// The merge invariant (internal/mergeproperty) for codegen's aggregate
// outputs.

// config_gen.k: one branch adds a field to the email block, the other a
// field (and a credential) to the stripe block. Each block's schema, env
// entries and secret refs are their own sections, with the nats block's in
// between untouched.
func TestMergeProperty_ConfigKCL(t *testing.T) {
	field := func(block, name string, sensitive bool) ConfigField {
		return ConfigField{
			Name: name, ProtoType: "string", GoType: "string",
			EnvVar: strings.ToUpper(name), Sensitive: sensitive,
			KCLBlock: block, KCLBlockType: strings.ToUpper(block[:1]) + block[1:] + "Config",
		}
	}
	root := ConfigField{Name: "log_level", ProtoType: "string", GoType: "string", EnvVar: "LOG_LEVEL", DefaultValue: "info"}
	fields := func(extraEmail, extraStripe bool) []ConfigField {
		fs := []ConfigField{root, field("email", "email_from", false), field("email", "resend_key", true)}
		if extraEmail {
			fs = append(fs, field("email", "email_reply_to", false))
		}
		fs = append(fs, field("nats", "nats_url", false), field("nats", "nats_password", true), field("stripe", "stripe_api_base", false))
		if extraStripe {
			fs = append(fs, field("stripe", "stripe_webhook_url", false))
		}
		return append(fs, field("stripe", "stripe_key", true))
	}
	variants := map[string][]ConfigField{
		"base": fields(false, false), "ours": fields(true, false),
		"theirs": fields(false, true), "merged": fields(true, true),
	}
	mergeproperty.AssertPlainMergeIsRender(t, "deploy/kcl/config_gen.k", func(v string) []byte {
		out, err := GenerateConfigKCL(variants[v], "app")
		if err != nil {
			t.Fatal(err)
		}
		return []byte(out)
	})
}

func TestMergeProperty_ReadOnlyGrants(t *testing.T) {
	table := func(name string, cols ...string) dbgrants.Table {
		tb := dbgrants.Table{Schema: "public", Name: name}
		for _, c := range cols {
			tb.Columns = append(tb.Columns, dbgrants.Column{Name: c})
		}
		return tb
	}
	// ours changes accounts; theirs changes users and adds widgets. Orders
	// sits between them, unchanged.
	tables := map[string][]dbgrants.Table{
		"base":   {table("accounts", "id"), table("orders", "id"), table("users", "id", "email")},
		"ours":   {table("accounts", "id", "balance"), table("orders", "id"), table("users", "id", "email")},
		"theirs": {table("accounts", "id"), table("orders", "id"), table("users", "id", "email", "name"), table("widgets", "id")},
		"merged": {table("accounts", "id", "balance"), table("orders", "id"), table("users", "id", "email", "name"), table("widgets", "id")},
	}
	for _, tc := range []struct {
		rel    string
		render func([]dbgrants.Table) (string, error)
		file   func(string) []byte
	}{
		{dbgrants.GrantsPath, dbgrants.Render, ReadOnlyGrantsFile},
		{dbgrants.DashboardGrantsPath, func(ts []dbgrants.Table) (string, error) {
			// Every table marked forge:dashboard, so the dashboard role sees them.
			marked := make([]dbgrants.Table, len(ts))
			for i, tb := range ts {
				tb.Dashboard = true
				marked[i] = tb
			}
			return dbgrants.RenderDashboard(marked)
		}, DashboardGrantsFile},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			mergeproperty.AssertPlainMergeIsRender(t, tc.rel, func(v string) []byte {
				script, err := tc.render(tables[v])
				if err != nil {
					t.Fatal(err)
				}
				return tc.file(script)
			})
		})
	}
}
