package cli

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/hostlaunch"
	"github.com/reliant-labs/forge/internal/storage"
)

// storageScheduleInstalledFn is the seam the notice reads through.
var storageScheduleInstalledFn = storageScheduleInstalled

// storageScheduleInstalled reports whether installStorageSchedule has run on
// this machine, by the presence of the unit file it writes. Checking the FILE
// rather than asking launchctl/systemctl keeps this cheap enough to call on
// every `forge env up` and keeps it from printing an error of its own on a
// platform where neither manager exists.
//
// An unreadable home directory, or any OS forge cannot schedule on, reports
// true: the notice's only purpose is to tell someone to run a command, and on a
// platform where that command prints "schedule it yourself" the notice is
// noise.
func storageScheduleInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true
	}
	switch runtime.GOOS {
	case "darwin":
		_, err = os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.reliant.forge-storage.plist"))
	case "linux":
		_, err = os.Stat(filepath.Join(home, ".config", "systemd", "user", "forge-storage.timer"))
	case "windows":
		// Task Scheduler has no unit file, so installWindowsTask leaves a copy
		// of the task XML under the user config dir for this check to find.
		marker, mErr := windowsTaskMarkerPath()
		if mErr != nil {
			return true
		}
		_, err = os.Stat(marker)
	default:
		return true
	}
	return err == nil
}

func installStorageSchedule(ctx context.Context, out io.Writer, path string, p storage.Policy) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		return fmt.Errorf("on %s schedule 'forge storage daemon --policy %s' with your service manager", runtime.GOOS, path)
	}
	r := storage.Runner{Policy: p}
	if err := r.Local(ctx); err != nil {
		return err
	}
	if p.DockerContext == "" {
		b, err := storage.Exec(ctx, "docker", "context", "show")
		if err != nil {
			return err
		}
		p.DockerContext = strings.TrimSpace(string(b))
	}
	if err := storage.Save(path, p); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(filepath.Dir(path), "bin")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tokens, err := forgeExecCommand()
	if err != nil {
		return err
	}
	src, err := os.Open(tokens[0])
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.CreateTemp(dir, ".forge-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(dst.Name()) }()
	if _, err = io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Chmod(0700); err != nil {
		_ = dst.Close()
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	executable := filepath.Join(dir, hostlaunch.ExeName(runtime.GOOS, "forge-storage"))
	if err := os.Rename(dst.Name(), executable); err != nil {
		if runtime.GOOS == "windows" {
			// Windows cannot replace an .exe that is currently executing.
			return fmt.Errorf("could not replace %s: the storage maintenance task is probably running right now; retry in a few minutes: %w", executable, err)
		}
		return err
	}
	args := append([]string{executable}, tokens[1:]...)
	args = append(args, "storage", "gc", "--policy", path, "--apply")
	switch runtime.GOOS {
	case "darwin":
		err = installLaunchAgent(ctx, args, home)
	case "windows":
		err = installWindowsTask(ctx, executable, args[1:])
	default:
		err = installSystemdTimer(ctx, args, home)
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Installed daily storage maintenance at 03:30 using %s; policy %s. Registry cleanup briefly interrupts pulls/pushes.\n", executable, path)
	return nil
}

func installLaunchAgent(ctx context.Context, args []string, home string) error {
	var err error

	var arguments strings.Builder
	for _, arg := range args {
		arguments.WriteString("<string>")
		_ = xml.EscapeText(&arguments, []byte(arg))
		arguments.WriteString("</string>")
	}
	logs := filepath.Join(home, "Library", "Logs", "forge-storage")
	if err := os.MkdirAll(logs, 0700); err != nil {
		return err
	}
	escape := func(s string) string { var b strings.Builder; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>Label</key><string>com.reliant.forge-storage</string><key>ProgramArguments</key><array>%s</array><key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>30</integer></dict><key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict><key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string><key>ExitTimeOut</key><integer>120</integer></dict></plist>`, arguments.String(), escape(os.Getenv("PATH")), escape(filepath.Join(logs, "maintenance.log")), escape(filepath.Join(logs, "errors.log")))
	file := filepath.Join(home, "Library", "LaunchAgents", "com.reliant.forge-storage.plist")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(plist), 0600); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_, _ = storage.Exec(ctx, "launchctl", "bootout", domain+"/com.reliant.forge-storage")
	if _, err = storage.Exec(ctx, "launchctl", "bootstrap", domain, file); err != nil {
		return err
	}
	return nil
}
func installSystemdTimer(ctx context.Context, args []string, home string) error {
	var err error

	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	var quoted []string
	for _, arg := range args {
		quoted = append(quoted, strconv.Quote(strings.ReplaceAll(arg, "%", "%%")))
	}
	service := "[Unit]\nDescription=Forge local storage maintenance\n[Service]\nType=oneshot\nTimeoutStartSec=30min\nTimeoutStopSec=120s\nExecStart=" + strings.Join(quoted, " ") + "\nEnvironment=" + strconv.Quote("PATH="+os.Getenv("PATH")) + "\n"
	timer := "[Unit]\nDescription=Daily Forge storage maintenance\n[Timer]\nOnCalendar=*-*-* 03:30:00\nPersistent=true\n[Install]\nWantedBy=timers.target\n"
	if err := os.WriteFile(filepath.Join(dir, "forge-storage.service"), []byte(service), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "forge-storage.timer"), []byte(timer), 0600); err != nil {
		return err
	}
	if _, err = storage.Exec(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if _, err = storage.Exec(ctx, "systemctl", "--user", "enable", "--now", "forge-storage.timer"); err != nil {
		return err
	}
	return nil
}
