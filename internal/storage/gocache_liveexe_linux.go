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
	"syscall"
	"time"
)

// procExecutables lists the executable of every running process that lives
// under root, from /proc/<pid>/exe. The kernel reports the resolved path, with
// " (deleted)" appended once the file is unlinked; that suffix is kept so
// callers see the process still holds a binary that is gone.
//
// A process that exits mid-scan (ENOENT) or has no executable (a kernel
// thread) is skipped. A process this user cannot inspect (EACCES, another
// user's) is skipped too: it is not ours to have started from this user's
// cache. A process of OUR user that cannot be inspected is not skippable: it
// might be running a cache entry, so the whole listing fails and the trim
// keeps every executable entry.
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
	uptime, uptimeErr := procUptime(proc)
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
			if errors.Is(err, fs.ErrPermission) && !ownedByUs(filepath.Join(proc, d.Name())) {
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
		if uptimeErr == nil {
			if a, ok := procAge(proc, d.Name(), uptime); ok {
				age = a
			}
		}
		out = append(out, LiveExecutable{Path: exe, Age: age})
	}
	return out, nil
}

func ownedByUs(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return true // cannot tell: treat as ours, which fails closed
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
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
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// clockTicksPerSecond is USER_HZ, which /proc/<pid>/stat reports start times
// in. It is 100 on every Linux ABI forge runs on.
const clockTicksPerSecond = 100

// procAge is how long process name has run: uptime minus its start time
// (field 22 of /proc/<pid>/stat, in clock ticks since boot).
func procAge(proc, name string, uptime time.Duration) (time.Duration, bool) {
	b, err := os.ReadFile(filepath.Join(proc, name, "stat"))
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
