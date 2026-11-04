package forgedelegate

import "context"

// entryLocks is false: there is no lock on Windows, so a staging directory
// found in an entry may be a live build's and is never swept.
const entryLocks = false

// lockEntry is a no-op on Windows: concurrent builds of one entry race, and
// publish keeps whichever binary lands first. A ctx that has ended still
// fails, as it does off Windows.
func lockEntry(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}
