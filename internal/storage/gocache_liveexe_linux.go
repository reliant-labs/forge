//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procExecutables lists the executable of every running process that lives
// under root, from /proc/<pid>/exe. The kernel reports the resolved path, with
// " (deleted)" appended once the file is unlinked; that suffix is kept so
// callers see the process still holds a binary that is gone.
//
// A process that exits mid-scan (ENOENT) or has no executable (a kernel
// thread) is skipped. So is a process whose exe the kernel will not show us
// (EACCES): readlink needs ptrace-read access, which the kernel refuses for
// another user's process and for ANY non-dumpable one, our own included (a
// runner agent, a setuid helper). A `go run` child is an ordinary dumpable
// process of this user, so an unreadable one is not it; failing the listing
// over it would instead disable executable trimming on every machine that
// runs one. What does fail the listing is /proc itself being unreadable, or
// any other error reading a process: the trim then keeps every executable
// entry.
func procExecutables(ctx context.Context, root string) ([]LiveExecutable, error) {
	return readProcExecutables(ctx, "/proc", root)
}

func readProcExecutables(ctx context.Context, proc, root string) ([]LiveExecutable, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", proc, err)
	}
	resolved := root
	if p, err := filepath.EvalSymlinks(root); err == nil {
		resolved = p
	}
	var out []LiveExecutable
	for _, d := range dirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(d.Name())
		if err != nil || !d.IsDir() {
			continue
		}
		exe, err := os.Readlink(filepath.Join(proc, d.Name(), "exe"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if errors.Is(err, fs.ErrPermission) {
				continue
			}
			return nil, fmt.Errorf("read the executable of process %d: %w", pid, err)
		}
		if _, ok := cacheEntryOfExecutable(resolved, exe); !ok {
			if _, ok := cacheEntryOfExecutable(root, exe); !ok {
				continue
			}
		}
		age := goCacheLongLived // unknown age counts as a build as old as any that counts
		if a, ok := procAge(proc, d.Name()); ok {
			age = a
		}
		out = append(out, LiveExecutable{Path: exe, Age: age})
	}
	return out, nil
}

// procUptime is the machine's uptime from /proc/uptime.
func procUptime(proc string) (time.Duration, error) {
	b, err := os.ReadFile(filepath.Join(proc, "uptime"))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("empty /proc/uptime")
	}
	// Kernel decimal seconds and start ticks share exact centisecond
	// boundaries. Float conversion can round 128.01s down by one nanosecond,
	// falsely making a newborn process's start time appear in the future.
	uptime, err := time.ParseDuration(f[0] + "s")
	if err != nil {
		return 0, err
	}
	if uptime < 0 {
		return 0, errors.New("negative /proc/uptime")
	}
	return uptime, nil
}

// clockTicksPerSecond is USER_HZ, which /proc/<pid>/stat reports start times
// in. It is 100 on every Linux ABI forge runs on.
const clockTicksPerSecond = 100

// procAge is how long process name has run: uptime minus its start time
// (field 22 of /proc/<pid>/stat, in clock ticks since boot). Sample uptime
// AFTER stat: a scan-wide snapshot can predate a newly started process and
// turn its negative computed age into the conservative unknown-age lease.
func procAge(proc, name string) (time.Duration, bool) {
	b, err := os.ReadFile(filepath.Join(proc, name, "stat"))
	if err != nil {
		return 0, false
	}
	uptime, err := procUptime(proc)
	if err != nil {
		return 0, false
	}
	return procAgeFromStat(string(b), uptime)
}

// procAgeFromStat parses a /proc/<pid>/stat line. The command name (field 2)
// is parenthesised and may contain spaces and parentheses, so fields are
// counted from the last ')'.
func procAgeFromStat(stat string, uptime time.Duration) (time.Duration, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return 0, false
	}
	ticks, err := strconv.ParseUint(f[19], 10, 64)
	if err != nil {
		return 0, false
	}
	age := uptime - time.Duration(ticks)*time.Second/clockTicksPerSecond
	if age < 0 {
		return 0, false
	}
	return age, true
}
