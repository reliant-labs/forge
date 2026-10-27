package storage

import "context"

// toolCacheUnlink charges exactly one descriptor-confined unlink to the
// existing pass governor. The tool cache owns traversal and the pinned parent;
// the governor must not reopen or chmod the display path.
func (d deleter) toolCacheUnlink(ctx context.Context, path string, remove func() error) error {
	d.pass.mu.Lock()
	defer d.pass.mu.Unlock()
	if err := d.grantLocked(ctx); err != nil {
		return err
	}
	return d.observeLocked(path, remove())
}
