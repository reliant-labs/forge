// Package authhooks receives auth events from an external identity provider
// (Supabase, Clerk, Auth0) and records them as forge's standard auth events
// (observe.TrackAuth): who signed up, signed in and signed out, reported by
// the issuer itself rather than by a browser.
//
// One endpoint per provider, POST /_forge/auth/hooks/<provider>, built by
// Handler over a Source. A Source verifies the sender (a bearer secret, or a
// Standard Webhooks signature) and normalises the provider's payload into
// Events. Handler then dedupes them and records each one. The route is public
// (the provider holds no user token) and authenticated by the hook's own
// secret: anything unverified, or a body over MaxBodyBytes, is answered 401
// with no detail. A verified payload it does not understand is answered 2xx,
// so the provider never retries it or disables the hook.
//
// It is a plain API: forge mounts nothing and reads no configuration. The
// app reads the secret from wherever it keeps secrets and mounts the handler
// on its own mux, outside its auth chain, conventionally at Path(provider):
//
//	mux.Handle(authhooks.Path("supabase"), authhooks.Supabase(authhooks.Options{Secret: secret}))
//
// The provider is self-reported: each endpoint records its events under its
// own name ("supabase", "clerk", "auth0"), and nothing is declared anywhere.
//
// Supabase has no outbound auth webhook that cannot block sign-in, so forge
// ships the provider side too: SupabaseSQL, triggers on auth.users and
// auth.sessions that post through pg_net, asynchronously, after commit. Their
// payload is the minimisation boundary: the user's opaque id, the sign-in
// provider's name, whether the account is verified or has a password, and a
// dedupe key. Never the email, IP address or user agent.
package authhooks
