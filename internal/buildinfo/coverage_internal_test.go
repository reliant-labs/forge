package buildinfo

import (
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/testkit"
)

func TestDiscoverDevForgeRootFromSource_OverrideSeam(t *testing.T) {
	t.Cleanup(ClearDiscoveredForgeRoot)

	SetDiscoveredForgeRoot("/pinned/forge")
	if got := DiscoverDevForgeRootFromSource(); got != "/pinned/forge" {
		t.Errorf("with an override set, got %q, want the pinned root", got)
	}

	// An empty override is a real answer ("not discoverable"), not "unset".
	SetDiscoveredForgeRoot("")
	if got := DiscoverDevForgeRootFromSource(); got != "" {
		t.Errorf("an empty override must disable discovery, got %q", got)
	}

	ClearDiscoveredForgeRoot()
	if got := DiscoverDevForgeRootFromSource(); got == "" {
		t.Skip("no on-disk source root discoverable (-trimpath test binary)")
	} else if !filepath.IsAbs(got) {
		t.Errorf("after Clear, discovery returned a non-absolute path %q", got)
	}
}

func TestIsModuleCachePath(t *testing.T) {
	sep := string(filepath.Separator)
	join := func(parts ...string) string { return sep + strings.Join(parts, sep) }
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"the extraction directory itself", join("go", "pkg", "mod", "github.com", "reliant-labs", "forge@v0.1.18"), true},
		{"a file deep inside an extraction", join("go", "pkg", "mod", "github.com", "reliant-labs", "forge@v0.1.18-0.20260926053138-05d5d6999d39", "internal", "x.go"), true},
		{"a checkout whose name has an @ but no version", join("src", "forge@work", "pkg"), false},
		{"a plain checkout", join("src", "forge", "pkg"), false},
		{"an @ with nothing before it", join("src", "@v1.2.3", "x"), false},
		{"the filesystem root", sep, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsModuleCachePath(tt.path); got != tt.want {
				t.Errorf("IsModuleCachePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestPublishedFloor_IsTheEmbeddedVersionFile(t *testing.T) {
	got := PublishedFloor()
	if got != versionFromFile(embeddedVersionFile) {
		t.Errorf("PublishedFloor = %q, want the validated embedded VERSION %q", got, versionFromFile(embeddedVersionFile))
	}
	if got == "" {
		t.Fatal("the embedded VERSION file must be a valid release tag")
	}
	if !releaseTagRE.MatchString(got) {
		t.Errorf("PublishedFloor %q is not a release tag", got)
	}
}

func TestGitCommit(t *testing.T) {
	prevV, prevC := version, gitCommit
	t.Cleanup(func() { Set(prevV, "", prevC) })

	Set("v1.0.0", "", "abc123def456")
	if got := GitCommit(); got != "abc123def456" {
		t.Errorf("a stamped commit must win, got %q", got)
	}

	// Unstamped ("unknown" or empty) falls back to build info, and when that
	// has no vcs.revision either, returns what was stamped.
	for _, stamped := range []string{"unknown", ""} {
		Set("v1.0.0", "", stamped)
		got := GitCommit()
		if info, ok := debug.ReadBuildInfo(); ok {
			want := stamped
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && s.Value != "" {
					want = s.Value
				}
			}
			if got != want {
				t.Errorf("stamped %q: GitCommit = %q, want %q", stamped, got, want)
			}
		}
	}
}

func TestSet_FeedsVersionAndCommit(t *testing.T) {
	prevV, prevC := version, gitCommit
	t.Cleanup(func() { Set(prevV, "", prevC) })

	Set("v9.8.7", "ignored-date", "feedfacecafe")
	if version != "v9.8.7" || gitCommit != "feedfacecafe" {
		t.Errorf("Set recorded (%q, %q)", version, gitCommit)
	}
}

func TestCheckoutVersion(t *testing.T) {
	testkit.Slow(t, "execs git")

	if got := CheckoutVersion(""); got != "" {
		t.Errorf("CheckoutVersion(\"\") = %q, want \"\"", got)
	}
	if got := CheckoutVersion(t.TempDir()); got != "" {
		t.Errorf("a non-repository must have no version, got %q", got)
	}

	repo := fixtureRepo(t, "v1.4.2")
	got := CheckoutVersion(repo)
	if got != deriveGitVersion(repo) {
		t.Errorf("CheckoutVersion = %q, want deriveGitVersion's %q", got, deriveGitVersion(repo))
	}
	// A pseudo-version for the commit at v1.4.2 is a pre-release of the next
	// patch, so it orders after the tag and before v1.4.3, and names a commit
	// (+dev), not a published module.
	if !strings.HasPrefix(got, "v1.4.3-0.") || !strings.HasSuffix(got, "+dev") {
		t.Errorf("CheckoutVersion at a tagged commit = %q, want v1.4.3-0.<ts>-<sha>+dev", got)
	}
}

func TestSetGitVersion_ControlsVersionFloor(t *testing.T) {
	t.Cleanup(ClearGitVersion)

	SetGitVersion("v3.2.1-0.20260101000000-abcdef123456+dev")
	if got := versionFloor("stamped"); got != "v3.2.1-0.20260101000000-abcdef123456+dev" {
		t.Errorf("a derivable git version must win, got %q", got)
	}

	SetGitVersion("")
	want := unknownDevAfter(versionFromFile(embeddedVersionFile))
	if got := versionFloor("stamped"); got != want {
		t.Errorf("with no git version the VERSION-file floor applies: got %q, want %q", got, want)
	}
	if !IsDevVersion(versionFloor("")) {
		t.Errorf("the VERSION-file floor %q must read as a dev version", versionFloor(""))
	}
}

func TestMarkDirty(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"v1.2.3", "v1.2.3+dirty"},
		{"v1.2.3+dev", "v1.2.3+dev"},
		{"v1.2.3+dirty", "v1.2.3+dirty"},
	}
	for _, tt := range tests {
		if got := markDirty(tt.in); got != tt.want {
			t.Errorf("markDirty(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildOrigin(t *testing.T) {
	tests := []struct {
		name string
		b    Build
		want string
	}{
		{"standalone", Build{}, "forge (standalone binary)"},
		{"embedded without a name", Build{Embedded: true}, "embedded forge"},
		{"embedded in a module path", Build{Embedded: true, Embedder: "github.com/reliant-labs/reliant"}, "reliant forge (embedded)"},
		{"embedded in a bare name", Build{Embedded: true, Embedder: "reliant"}, "reliant forge (embedded)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.Origin(); got != tt.want {
				t.Errorf("Origin = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildStringClauses(t *testing.T) {
	tests := []struct {
		name string
		b    Build
		want string
	}{
		{"version only", Build{Version: "v1.2.3"}, "v1.2.3"},
		{"unknown commit is omitted", Build{Version: "v1.2.3", Commit: "unknown"}, "v1.2.3"},
		{"commit shortened to 12", Build{Version: "v1.2.3", Commit: "750833054b78ab216150bdaad19c389cb91cbc53"}, "v1.2.3 (commit 750833054b78)"},
		{"build date", Build{Version: "v1.2.3", BuildDate: "2026-11-26"}, "v1.2.3 (built 2026-11-26)"},
		{
			"embedded with everything",
			Build{Version: "v1.2.3", Embedded: true, Embedder: "reliant", EmbedderVersion: "v5.0.0", EmbedderCommit: "0359a80a8f2311522b41550859e9c6e827efefc5", ReplacedBy: "../forge"},
			"v1.2.3 (embedded in reliant v5.0.0 @0359a80a8f23, forge => ../forge)",
		},
		{"embedded in an unnamed host", Build{Version: "v1.2.3", Embedded: true}, "v1.2.3 (embedded in another binary)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.String(); got != tt.want {
				t.Errorf("String = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSetBuildDate_ReachesCurrent(t *testing.T) {
	t.Cleanup(func() { SetBuildDate("") })

	SetBuildDate("  2026-11-26T10:00:00Z  ")
	if got := Current().BuildDate; got != "2026-11-26T10:00:00Z" {
		t.Errorf("BuildDate = %q, want the trimmed stamp", got)
	}
	SetBuildDate("unknown")
	if got := Current().BuildDate; got != "" {
		t.Errorf("the literal \"unknown\" must read as no date, got %q", got)
	}
	SetBuildDate("")
	if got := Current().BuildDate; got != "" {
		t.Errorf("an empty stamp must be omitted, got %q", got)
	}
}

func TestCurrentIdentityAndDescribe(t *testing.T) {
	prevV, prevC := version, gitCommit
	t.Cleanup(func() { Set(prevV, "", prevC); SetBuildDate("") })

	Set("v7.7.7", "", "0123456789abcdef0123")
	SetBuildDate("2026-11-26")

	cur := Current()
	if cur.Version != "v7.7.7" || cur.Commit != "0123456789abcdef0123" {
		t.Errorf("ldflags stamps must win in Current: %+v", cur)
	}
	if id := Identity(); id != cur.String() || !strings.HasPrefix(id, "v7.7.7") || !strings.Contains(id, "commit 0123456789ab") || !strings.Contains(id, "built 2026-11-26") {
		t.Errorf("Identity = %q", id)
	}

	d := Describe()
	for _, want := range []string{"forge build:    v7.7.7", "commit:         0123456789ab", "invoked as:     "} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "embedded in:") && !cur.Embedded {
		t.Errorf("a standalone build must not describe an embedder:\n%s", d)
	}
}

func TestDescribe_EmbeddedLines(t *testing.T) {
	// Describe reads Current(); under go test forge is standalone, so the
	// embedded rendering is pinned through the same Build fields it prints.
	b := Build{Version: "v1.0.0", Embedded: true, Embedder: "github.com/reliant-labs/reliant", EmbedderVersion: "v5.0.0", EmbedderCommit: "0359a80a8f2311522b41550859e9c6e827efefc5", ReplacedBy: "../forge"}
	if got := b.Origin(); got != "reliant forge (embedded)" {
		t.Errorf("Origin = %q", got)
	}
	if got := b.String(); !strings.Contains(got, "embedded in github.com/reliant-labs/reliant v5.0.0 @0359a80a8f23") || !strings.Contains(got, "forge => ../forge") {
		t.Errorf("String = %q", got)
	}
}

func TestForgeSourceAndLocalBuild_AmbientBinary(t *testing.T) {
	prevV, prevC := version, gitCommit
	t.Cleanup(func() { Set(prevV, "", prevC) })
	Set("v1.0.0", "", "stampedcommit00")

	src := ForgeSource()
	want := sourceFrom(rawBuildInfo(), "stampedcommit00")
	if src != want {
		t.Errorf("ForgeSource = %+v, want sourceFrom of the ambient build info %+v", src, want)
	}
	if got := LocalBuild(); got != localBuildFrom(rawBuildInfo()) {
		t.Errorf("LocalBuild = %v disagrees with localBuildFrom", got)
	}
}

func TestSourceRoot_FollowsTheDiscoverySeam(t *testing.T) {
	t.Cleanup(ClearDiscoveredForgeRoot)
	prev := DevForgeRoot
	t.Cleanup(func() { DevForgeRoot = prev })

	DevForgeRoot = ""
	SetDiscoveredForgeRoot("/pinned/forge")
	if got := SourceRoot(); got != "/pinned/forge" {
		t.Errorf("SourceRoot = %q, want the discovered root", got)
	}
	DevForgeRoot = "/stamped/forge"
	if got := SourceRoot(); got != "/stamped/forge" {
		t.Errorf("SourceRoot = %q, want the ldflags-stamped root to win", got)
	}
}

func TestResolveForgeRoot_ImplicitSources(t *testing.T) {
	t.Setenv(ForgeRootEnv, "")
	t.Cleanup(ClearDiscoveredForgeRoot)
	prev := DevForgeRoot
	t.Cleanup(func() { DevForgeRoot = prev })

	t.Run("a build-time stamp is the first implicit source", func(t *testing.T) {
		DevForgeRoot = "/stamped/forge"
		got, ok, err := ResolveForgeRoot("")
		if err != nil || !ok || got.Dir != "/stamped/forge" || !strings.Contains(got.Source, "stamped") {
			t.Errorf("got (%+v, %v, %v)", got, ok, err)
		}
	})

	t.Run("a pinned discovery stands for the remaining sources", func(t *testing.T) {
		DevForgeRoot = ""
		SetDiscoveredForgeRoot("")
		got, ok, err := ResolveForgeRoot("")
		// SetDiscoveredForgeRoot("") makes DiscoverDevForgeRootFromSource
		// return "", so ResolveForgeRoot reports the pinned "nothing found".
		if err != nil || ok || got.Dir != "" {
			t.Errorf("a pinned empty discovery must resolve to nothing, got (%+v, %v, %v)", got, ok, err)
		}
	})

	t.Run("a pinned root is returned from the discovery step", func(t *testing.T) {
		DevForgeRoot = ""
		SetDiscoveredForgeRoot("/pinned/forge")
		got, ok, err := ResolveForgeRoot("")
		if err != nil || !ok || got.Dir != "/pinned/forge" {
			t.Errorf("got (%+v, %v, %v)", got, ok, err)
		}
	})

	t.Run("with nothing pinned the live checkout is found", func(t *testing.T) {
		DevForgeRoot = ""
		ClearDiscoveredForgeRoot()
		got, ok, err := ResolveForgeRoot("")
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Skip("no checkout reachable from this test binary (-trimpath build run outside the tree)")
		}
		if back := forgeRootFromFile(filepath.Join(got.Dir, "x")); back != got.Dir {
			t.Errorf("resolved root %q is not a forge checkout (forgeRootFromFile gives %q)", got.Dir, back)
		}
		if got.Source == "" {
			t.Error("a resolved root must say how it was found")
		}
	})
}

func TestResolveForgeRoot_EnvPathIsAnErrorWhenNotACheckout(t *testing.T) {
	bad := t.TempDir()
	t.Setenv(ForgeRootEnv, bad)
	_, ok, err := ResolveForgeRoot("")
	if err == nil || ok {
		t.Fatalf("a $FORGE_ROOT that is not a checkout must be an error, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "not a forge checkout") {
		t.Errorf("error %q must name the offending path", err)
	}
}

func TestSearchBases(t *testing.T) {
	sep := string(filepath.Separator)
	exe := filepath.Join(sep+"a", "b", "bin", "reliant")
	start := filepath.Join(sep+"a", "b", "work", "x")
	got := searchBases(exe, start, "")

	want := []string{
		filepath.Join(sep+"a", "b", "bin"),
		filepath.Join(sep+"a", "b"),
		sep + "a",
		filepath.Join(sep+"a", "b", "work"),
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			t.Errorf("searchBases missing %q in %v", w, got)
		}
	}
	seen := map[string]bool{}
	for _, g := range got {
		if seen[g] {
			t.Errorf("searchBases returned %q twice: %v", g, got)
		}
		seen[g] = true
	}
}

func TestForgeBytesFromCheckout(t *testing.T) {
	standalone := func(version string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Path: forgeModulePath, Version: version}, Settings: settings}
	}
	embedded := func(dep debug.Module, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main:     debug.Module{Path: "github.com/reliant-labs/reliant", Version: "v5.0.0"},
			Deps:     []*debug.Module{&dep},
			Settings: settings,
		}
	}
	vcs := debug.BuildSetting{Key: "vcs.revision", Value: "abc"}
	tests := []struct {
		name string
		info *debug.BuildInfo
		want bool
	}{
		{"standalone with a vcs stamp was built from a repo", standalone("v1.2.3", vcs), true},
		{"standalone from the module cache carries no vcs", standalone("v1.2.3"), false},
		{"embedded, directory replace", embedded(debug.Module{Path: forgeModulePath, Version: "v1.2.3", Replace: &debug.Module{Path: "../forge"}}), true},
		{"embedded, devel", embedded(debug.Module{Path: forgeModulePath, Version: "(devel)"}), true},
		{"embedded, version with a go.sum hash is a proxy module", embedded(debug.Module{Path: forgeModulePath, Version: "v1.2.3", Sum: "h1:abc"}, vcs), false},
		{"embedded, the host's vcs stamps say nothing about forge", embedded(debug.Module{Path: forgeModulePath, Version: "v1.2.3", Sum: "h1:abc"}, vcs, debug.BuildSetting{Key: "vcs.modified", Value: "true"}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := forgeBytesFromCheckout(tt.info); got != tt.want {
				t.Errorf("forgeBytesFromCheckout = %v, want %v", got, tt.want)
			}
		})
	}
}
