package authhooks

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/observe"
	"github.com/reliant-labs/forge/pkg/pgtest"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// fakeSupabase stands up what the SQL touches in a Supabase project: the
// auth tables' relevant columns, Vault's decrypted view (a table here), and
// pg_net's http_post (a stub that records each request instead of sending
// it, which is all the triggers can observe of pg_net anyway).
const fakeSupabase = `
create schema auth;
create table auth.users (
  id uuid primary key,
  email text,
  phone text,
  encrypted_password text,
  email_confirmed_at timestamptz,
  raw_app_meta_data jsonb,
  raw_user_meta_data jsonb,
  is_anonymous boolean not null default false
);
create table auth.sessions (id uuid primary key, user_id uuid not null references auth.users(id), ip inet, user_agent text);
create schema vault;
create table vault.decrypted_secrets (name text primary key, decrypted_secret text);
create schema net;
create table net.sent (url text, body jsonb, headers jsonb);
create function net.http_post(url text, body jsonb default '{}', params jsonb default '{}',
  headers jsonb default '{}', timeout_milliseconds int default 5000) returns bigint
language plpgsql as $$
begin
  insert into net.sent values (url, body, headers);
  return 1;
end $$;
`

const pgNetLine = "create extension if not exists pg_net;"

func TestSupabaseSQLAgainstPostgres(t *testing.T) {
	testkit.Slow(t, "boots a real postgres (pgtest) to run the Supabase trigger SQL")
	db, cleanup, err := pgtest.New()
	if err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(cleanup)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(fakeSupabase)
	script := SupabaseSQL()
	if !strings.Contains(script, pgNetLine) {
		t.Fatalf("the SQL no longer enables pg_net with %q", pgNetLine)
	}
	// pg_net is not in a plain postgres; the stub above stands in for it.
	script = strings.Replace(script, pgNetLine, "", 1)
	exec(script)
	exec(script) // idempotent: a second run upgrades in place

	sent := func() []map[string]any {
		t.Helper()
		rows, err := db.QueryContext(ctx, `select body, headers from net.sent`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []map[string]any
		for rows.Next() {
			var body, headers []byte
			if err := rows.Scan(&body, &headers); err != nil {
				t.Fatal(err)
			}
			var b, h map[string]any
			_ = json.Unmarshal(body, &b)
			_ = json.Unmarshal(headers, &h)
			if h["authorization"] != "Bearer hook-secret" {
				t.Errorf("request headers = %v, want the Vault secret as a bearer", h)
			}
			out = append(out, b)
		}
		exec(`delete from net.sent`)
		return out
	}

	const realUser = "11111111-1111-4111-8111-111111111111"
	// No secrets in Vault yet: sign-ups work and nothing is sent.
	exec(`insert into auth.users (id, email, encrypted_password, raw_app_meta_data) values ($1, 'a@example.com', 'x', '{"provider":"email","providers":["email"]}')`, realUser)
	if got := sent(); len(got) != 0 {
		t.Fatalf("sent %v with no hook configured", got)
	}

	exec(`insert into vault.decrypted_secrets values ('forge_auth_hook_url', 'https://app.example/_forge/auth/hooks/supabase'), ('forge_auth_hook_secret', 'hook-secret')`)

	// An anonymous visitor is not a sign-up, and their session not a sign-in.
	const anon = "22222222-2222-4222-8222-222222222222"
	exec(`insert into auth.users (id, is_anonymous, raw_app_meta_data) values ($1, true, '{"provider":"anonymous","providers":["anonymous"]}')`, anon)
	exec(`insert into auth.sessions (id, user_id, ip, user_agent) values (gen_random_uuid(), $1, '203.0.113.9', 'Mozilla')`, anon)
	if got := sent(); len(got) != 0 {
		t.Fatalf("sent %v for an anonymous user", got)
	}

	// Upgrading the anonymous user (linking an email, no password) is the sign-up.
	exec(`update auth.users set is_anonymous = false, email = 'b@example.com', email_confirmed_at = now(),
		raw_app_meta_data = '{"provider":"anonymous","providers":["anonymous","email"]}' where id = $1`, anon)
	got := sent()
	if len(got) != 1 || got[0]["event"] != "signed_up" || got[0]["user_id"] != anon ||
		got[0]["provider"] != "email" || got[0]["password"] != false || got[0]["verified"] != true {
		t.Fatalf("upgrade sent %v, want one signed_up for %s via email, verified, no password", got, anon)
	}
	// Updating anything else is not another sign-up.
	exec(`update auth.users set is_anonymous = false, email = 'c@example.com' where id = $1`, anon)
	if got := sent(); len(got) != 0 {
		t.Fatalf("a second update sent %v", got)
	}

	// A new session of a real user is a sign-in, keyed by the session.
	const session = "33333333-3333-4333-8333-333333333333"
	exec(`insert into auth.sessions (id, user_id, ip, user_agent) values ($1, $2, '203.0.113.9', 'Mozilla')`, session, realUser)
	got = sent()
	if len(got) != 1 || got[0]["event"] != "signed_in" || got[0]["dedupe"] != session {
		t.Fatalf("session sent %v, want one signed_in keyed by %s", got, session)
	}
	for _, b := range got {
		for k := range b {
			if !slices.Contains(payloadKeys, k) {
				t.Errorf("payload key %q is outside %v", k, payloadKeys)
			}
		}
		if s, _ := json.Marshal(b); strings.Contains(string(s), "example.com") || strings.Contains(string(s), "203.0.113.9") || strings.Contains(string(s), "Mozilla") {
			t.Errorf("payload carries personal data: %s", s)
		}
	}

	// What the SQL sends is what the endpoint reads.
	body, _ := json.Marshal(got[0])
	evs, err := supabaseSource{}.Events(nil, body)
	if err != nil || len(evs) != 1 || evs[0].Kind != observe.AuthSignedIn || evs[0].UserID != realUser || evs[0].DedupeKey != session {
		t.Errorf("the endpoint read %+v, %v from %s", evs, err, body)
	}

	// A failing pg_net never fails auth.
	exec(`create or replace function net.http_post(url text, body jsonb default '{}', params jsonb default '{}',
		headers jsonb default '{}', timeout_milliseconds int default 5000) returns bigint
		language plpgsql as $$ begin raise exception 'pg_net is down'; end $$`)
	exec(`insert into auth.users (id, raw_app_meta_data) values (gen_random_uuid(), '{"provider":"google","providers":["google"]}')`)
	var n int
	if err := db.QueryRowContext(ctx, `select count(*) from auth.users`).Scan(&n); err != nil || n != 3 {
		t.Errorf("users = %d, %v; the sign-up must succeed while pg_net fails", n, err)
	}
}
