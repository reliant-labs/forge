package cli

import (
	"archive/tar"
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// writeELF writes the smallest file debug/elf accepts as a 64-bit little-endian
// Linux executable for goarch. The image-binary guard reads only the header, so
// a real compile (seconds, a toolchain) would add nothing to these tests.
func writeELF(t *testing.T, path, goarch string) {
	t.Helper()
	machine, ok := goarchToELFMachine(goarch)
	if !ok {
		t.Fatalf("no ELF machine for %q", goarch)
	}
	var b bytes.Buffer
	hdr := elf.Header64{
		Ident: [elf.EI_NIDENT]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT), byte(elf.ELFOSABI_NONE)},
		Type:  uint16(elf.ET_EXEC), Machine: uint16(machine), Version: uint32(elf.EV_CURRENT),
		Ehsize: 64, Phentsize: 56, Shentsize: 64,
	}
	if err := binary.Write(&b, binary.LittleEndian, &hdr); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fixtureBase is a stand-in alpine: a CA bundle, so the Dockerfile's
// `apk add ca-certificates` is satisfiable, and /app, so WORKDIR /app adds no
// directory entry.
func fixtureBase(t *testing.T) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name, body string
		dir        bool
	}{
		{"etc/", "", true}, {"etc/ssl/", "", true}, {"etc/ssl/certs/", "", true},
		{"etc/ssl/certs/ca-certificates.crt", "-----BEGIN CERTIFICATE-----", false},
		{"app/", "", true},
	} {
		h := &tar.Header{Name: f.name, Mode: 0o755, Typeflag: tar.TypeReg, Size: int64(len(f.body))}
		if f.dir {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(buf.Bytes(), types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.MediaType(img, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.OCIConfigJSON)
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	diff, err := layers[0].DiffID()
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.ConfigFile(img, &v1.ConfigFile{
		OS: "linux", Architecture: "amd64",
		RootFS: v1.RootFS{Type: "layers", DiffIDs: []v1.Hash{diff}},
		Config: v1.Config{Env: []string{"PATH=/usr/bin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}
