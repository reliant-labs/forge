package cli

import (
	"os"
	"os/exec"
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