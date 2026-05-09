package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func isPluginAvailable(pluginName string) bool {
	_, err := exec.LookPath(pluginName)
	return err == nil
}

// discoverProtoSubdirs returns project-relative paths of every immediate
// subdir under proto/ that contains at least one .proto file. The result
// is sorted for determinism.
//
// This unblocks pack-emitted protos that live outside the canonical
// proto/{services,api,db,config} layout (e.g. proto/audit/ from the
// audit-log pack, proto/api_key/ from the api-key pack). Without this,
// the descriptor walk and frontend TS generation silently drop those
// services, which surfaces as "missing useListAuditEvents hook" downstream.
func discoverProtoSubdirs(projectDir string) []string {
	root := filepath.Join(projectDir, "proto")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join("proto", e.Name())
		has, err := hasProtoFilesInDir(filepath.Join(projectDir, sub))
		if err != nil || !has {
			continue
		}
		dirs = append(dirs, sub)
	}
	sort.Strings(dirs)
	return dirs
}