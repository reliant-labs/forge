package forgedelegate

import "context"

// entryLocks is false: there is no lock on Windows, so a staging directory
// found in an entry may be a live build's and is never swept.
const entryLocks = false

// lockEntry is a no-op on Windows: concurrent builds of one entry race, and
// publish keeps whichever binary lands first.
func lockEntry(context.Context, string) (func(), error) { return func() {}, nil }
