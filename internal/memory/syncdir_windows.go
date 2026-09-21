package memory

// syncDirectory does nothing on Windows: NTFS journals metadata, and fsync
// on a directory handle fails.
func syncDirectory(string) error { return nil }
