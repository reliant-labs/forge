//go:build linux

package cli

import (
	"os"
	"strconv"
)

// readProcStamp reads pid's process group and start time from
// /proc/<pid>/stat.
func readProcStamp(pid int) (procStamp, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStamp{}, false
	}
	return parseProcStatStamp(data)
}

// startMark is now, on the clock /proc/<pid>/stat's starttime counts on (time
// since boot), from /proc/uptime.
func startMark() (int64, bool) {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	return parseUptimeMark(data)
}
