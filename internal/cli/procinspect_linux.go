//go:build linux

package cli

import (
	"os"
	"strconv"
	"strings"
)

// readProcEnviron returns pid's environment as KEY=VALUE strings on Linux
// by reading /proc/<pid>/environ (NUL-separated). ok is false when the file
// is unreadable (dead pid, or a process owned by another uid whose environ
// the kernel withholds) — an unreadable holder is treated as NOT-forge-owned
// so an unidentifiable process is never reclaimed.
func readProcEnviron(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return nil, false
	}
	var out []string
	for _, kv := range strings.Split(string(data), "\x00") {
		if kv != "" {
			out = append(out, kv)
		}
	}
	return out, true
}
