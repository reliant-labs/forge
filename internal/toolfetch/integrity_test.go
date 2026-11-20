package toolfetch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCacheRejectsSubstitution(t *testing.T) {
	t.Parallel()
	for _, attack := range []string{"binary", "archive", "source"} {
		t.Run(attack, func(t *testing.T) {
			t.Parallel()
			archive := tarGz(t, "faketool", fakeBinary)
			tool := fakeTool(archive, nil)
			rel := newRelease(t)
			rel.serve(tool, "linux", "amd64", archive)
			f := rel.fetcher(t, "linux")
			in, err := f.Ensure(context.Background(), tool)
			if err != nil {
				t.Fatal(err)
			}
			switch attack {
			case "binary":
				err = os.WriteFile(in.Path, []byte("substituted"), 0o755)
			case "archive":
				err = os.WriteFile(filepath.Join(filepath.Dir(in.Path), tool.Asset("linux", "amd64")), tarGz(t, "faketool", "evil"), 0o600)
			case "source":
				in.Source = SourceGoInstall
				in.GoPackage = tool.GoPackage + "@" + tool.Version
				err = writeManifest(filepath.Dir(in.Path), in)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := f.Find(tool); ok {
				t.Fatal("Find trusted a substituted entry")
			}
			fixed, err := f.Ensure(context.Background(), tool)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(fixed.Path)
			if err != nil || string(got) != fakeBinary || fixed.Source != SourceRelease {
				t.Fatalf("repair = %q, %+v, %v", got, fixed, err)
			}
			if rel.hits.Load() != 2 {
				t.Fatal("Ensure did not reinstall")
			}
		})
	}
}

func TestUnsafeMetadataRejectedBeforeIO(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"name", "version", "os", "arch", "asset"} {
		for _, bad := range []string{"..", "../escape", `..\escape`, "/absolute", `C:drive`, "trailing.", "nul\x00"} {
			t.Run(field+"/"+bad, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				f := testFetcher(filepath.Join(root, "cache"), "", "linux")
				f.LookPath = func(string) (string, error) { t.Fatal("unexpected IO"); return "", nil }
				tool := Tool{Name: "faketool", Version: "v1", Checksums: map[string]string{}}
				switch field {
				case "name":
					tool.Name = bad
				case "version":
					tool.Version = bad
				case "os":
					f.GOOS = bad
				case "arch":
					f.GOARCH = bad
				case "asset":
					tool.Checksums[bad] = "bad"
				}
				if _, err := f.Ensure(context.Background(), tool); err == nil {
					t.Fatal("Ensure accepted unsafe metadata")
				}
				if _, ok := f.Find(tool); ok {
					t.Fatal("Find accepted unsafe metadata")
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 0 {
					t.Fatalf("cache touched: %v, %v", entries, err)
				}
			})
		}
	}
}
