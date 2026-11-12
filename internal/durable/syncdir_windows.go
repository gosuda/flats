package durable

// SyncDir does nothing on Windows: a directory handle cannot be flushed, and
// NTFS journals its metadata itself.
func SyncDir(string) error { return nil }
