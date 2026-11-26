package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDescriptorRefreshPolicyDoesNotLeakToConcurrentReaders(t *testing.T) {
	quietDir, verboseDir := t.TempDir(), t.TempDir()
	for _, dir := range []string{quietDir, verboseDir} {
		proto := filepath.Join(dir, "proto", "test.proto")
		if err := os.MkdirAll(filepath.Dir(proto), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(proto, []byte("syntax = \"proto3\";\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldBuilder, oldPolicyBuilder := descriptorBuilder, descriptorContextBuilder
	t.Cleanup(func() { descriptorBuilder, descriptorContextBuilder = oldBuilder, oldPolicyBuilder })
	started, release := make(chan struct{}), make(chan struct{})
	seen := make(chan bool, 2)
	RegisterDescriptorPolicyBuilder(func(dir string, quiet bool) error {
		if dir == quietDir {
			close(started)
			<-release
		}
		seen <- quiet
		return WriteDescriptor(dir, nil)
	})
	done := make(chan error, 1)
	go func() {
		_, err := LoadDescriptorWithQuiet(quietDir, true)
		done <- err
	}()
	<-started
	_, verboseErr := LoadDescriptor(verboseDir)
	close(release)
	quietErr := <-done
	if verboseErr != nil || quietErr != nil {
		t.Fatalf("verbose=%v quiet=%v", verboseErr, quietErr)
	}
	if verboseQuiet, quietQuiet := <-seen, <-seen; verboseQuiet || !quietQuiet {
		t.Fatalf("verbose quiet=%v, quiet quiet=%v", verboseQuiet, quietQuiet)
	}
}
