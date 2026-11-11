package templates

import "strings"

// javaKeywords are reserved words Android refuses as a package segment
// (com.new.app does not build).
var javaKeywords = map[string]bool{
	"abstract": true, "assert": true, "boolean": true, "break": true, "byte": true,
	"case": true, "catch": true, "char": true, "class": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extends": true, "false": true, "final": true, "finally": true,
	"float": true, "for": true, "goto": true, "if": true, "implements": true,
	"import": true, "instanceof": true, "int": true, "interface": true, "long": true,
	"native": true, "new": true, "null": true, "package": true, "private": true,
	"protected": true, "public": true, "return": true, "short": true, "static": true,
	"strictfp": true, "super": true, "switch": true, "synchronized": true, "this": true,
	"throw": true, "throws": true, "transient": true, "true": true, "try": true,
	"void": true, "volatile": true, "while": true,
}

// appIDSegment reduces one name to a segment every native identifier format
// accepts: lowercase [a-z0-9] only, starting with a letter, not a Java keyword.
// Android package segments forbid '-', URI schemes forbid '_', so the
// intersection (no separators at all) is the only spelling that can be the
// URI scheme, the iOS bundle id and the Android package at once.
func appIDSegment(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "app" + s
	}
	if javaKeywords[s] {
		s += "app"
	}
	return s
}

// NativeAppID is a native frontend's reverse-DNS app id, com.<project>.<frontend>,
// valid as iOS bundleIdentifier, Android package and RFC 8252 private-use URI
// scheme alike (the sign-in redirect is "<id>:/oauthredirect"). app.json uses
// it as the one expo.scheme too: a bare frontend-name scheme would collide
// across apps on a device, and a second entry makes expo-linking warn. Project and
// frontend names allow uppercase, '_' and '-', none of which survive in all
// three, so each is sanitised (see appIDSegment).
func NativeAppID(project, frontend string) string {
	return "com." + appIDSegment(project) + "." + appIDSegment(frontend)
}

// DNSLabel reduces a name to one DNS label: lowercase, every run of characters
// outside [a-z0-9] collapsed to a single '-', no leading or trailing '-'. The
// dev IdP derives its instance domain from InstanceName, and Zitadel refuses
// to boot on a character a domain cannot hold (a project named "my_app").
func DNSLabel(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		return "app"
	}
	return s
}
