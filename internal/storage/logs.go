package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

var rotatedLog = regexp.MustCompile(`^(.+)\.[0-9]{4}-[0-9]{2}-[0-9]{2}T[^/]+\.log$`)

// Logs only expires rotated Forge logs. Current streams, releases, promotion
// history and other files in .forge are never candidates.
func (r Runner) Logs(apply bool) error {
	for _, project := range r.Policy.Projects {
		dirs, err := os.ReadDir(filepath.Join(project, ".forge", "logs"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, dir := range dirs {
			if !dir.IsDir() {
				continue
			}
			root := filepath.Join(project, ".forge", "logs", dir.Name())
			entries, err := os.ReadDir(root)
			if err != nil {
				return err
			}
			type logFile struct {
				name, stream string
				size         int64
				at           time.Time
			}
			var files []logFile
			var total int64
			for _, entry := range entries {
				match := rotatedLog.FindStringSubmatch(entry.Name())
				if len(match) != 2 || !entry.Type().IsRegular() {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				files = append(files, logFile{entry.Name(), match[1], info.Size(), info.ModTime()})
				total += info.Size()
			}
			sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
			count := map[string]int{}
			var candidates []logFile
			for _, f := range files {
				count[f.stream]++
				if count[f.stream] > 5 {
					candidates = append(candidates, f)
				}
			}
			for i := len(candidates) - 1; i >= 0; i-- {
				f := candidates[i]
				if time.Since(f.at) < 7*24*time.Hour && total <= int64(r.Policy.LogBudgetGiB*GiB) {
					continue
				}
				path := filepath.Join(root, f.name)
				r.print("expire rotated log %s (%d bytes)\n", path, f.size)
				if apply {
					if err := os.Remove(path); err != nil {
						return fmt.Errorf("remove rotated log: %w", err)
					}
				}
				total -= f.size
			}
		}
	}
	return nil
}

// RegisterProject adds a project whose rotated logs Forge can maintain.
func RegisterProject(path, project string) error {
	absolute, err := filepath.Abs(project)
	if err != nil {
		return err
	}
	return WithLock(path, func() error {
		p, err := Load(path)
		if err != nil {
			return err
		}
		if !contains(p.Projects, absolute) {
			p.Projects = append(p.Projects, absolute)
		}
		return Save(path, p)
	})
}
