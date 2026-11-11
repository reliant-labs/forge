package authhooks

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Supabase SQL is the privacy boundary: whatever it posts leaves the
// auth database. This pins the keys of every payload it builds to the ones
// the endpoint reads, and the columns it reads them from to non-personal
// ones.

// payloadKeys are the only keys forge_auth.emit may be given.
var payloadKeys = []string{"v", "event", "user_id", "at", "provider", "password", "verified", "dedupe"}

func TestSupabaseSQLSendsOnlyTheMinimalKeys(t *testing.T) {
	sql := SupabaseSQL()
	calls := regexp.MustCompile(`(?s)forge_auth\.emit\(jsonb_build_object\((.*?)\)\);`).FindAllStringSubmatch(sql, -1)
	if len(calls) != 2 {
		t.Fatalf("found %d forge_auth.emit(jsonb_build_object(...)) payloads, want 2 (on_user, on_session)", len(calls))
	}
	keyRE := regexp.MustCompile(`(?m)^\s*'([a-z_]+)',`)
	for _, c := range calls {
		keys := keyRE.FindAllStringSubmatch(c[1], -1)
		if len(keys) == 0 {
			t.Fatalf("payload has no keys:\n%s", c[1])
		}
		for _, k := range keys {
			if !slices.Contains(payloadKeys, k[1]) {
				t.Errorf("the SQL sends key %q, outside %v", k[1], payloadKeys)
			}
		}
		for _, personal := range []string{"email", "phone", "ip", "user_agent", "raw_user_meta_data"} {
			if regexp.MustCompile(`\.` + personal + `\b`).MatchString(c[1]) {
				t.Errorf("a payload reads %q:\n%s", personal, c[1])
			}
		}
	}
}

func TestSupabaseSQLNeverFailsAuth(t *testing.T) {
	sql := SupabaseSQL()
	// Every function that runs inside an auth transaction swallows its
	// errors, and the request goes through pg_net (queued, sent after
	// commit), never a synchronous http call.
	for _, fn := range []string{"forge_auth.emit", "forge_auth.on_user", "forge_auth.on_session"} {
		i := strings.Index(sql, "create or replace function "+fn+"(")
		if i < 0 {
			t.Fatalf("%s is not defined", fn)
		}
		body := sql[i:]
		body = body[:strings.Index(body, "end $$;")]
		if !strings.Contains(body, "exception when others then") {
			t.Errorf("%s does not swallow its errors", fn)
		}
		if !strings.Contains(body, "security definer set search_path = ''") {
			t.Errorf("%s is not security definer with an empty search_path", fn)
		}
	}
	if !strings.Contains(sql, "net.http_post(") || strings.Contains(sql, "extensions.http") {
		t.Error("events must go through pg_net (async), not the synchronous http extension")
	}
	if strings.Contains(strings.ToLower(sql), "create or replace function auth.") {
		t.Error("functions may not live in the auth schema (Supabase refuses it)")
	}
}
