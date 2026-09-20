package loop

// syncDirectory does nothing on Windows, and the reason is worth stating
// rather than leaving as an empty function.
//
// There is no equivalent of fsync on a directory handle: FlushFileBuffers
// against one fails, so Go's File.Sync does too. The durability of the rename
// itself comes from the filesystem — NTFS journals metadata, so the directory
// entry is recovered or not by the journal rather than by anything this
// process can ask for. MoveFileEx accepts MOVEFILE_WRITE_THROUGH, which would
// wait for that metadata, but os.Rename does not pass it and reaching past it
// would mean reimplementing the rename.
//
// So the guarantee here is weaker than on Unix, and README and SECURITY.md say
// so rather than implying one rule for every platform.
func syncDirectory(string) error { return nil }
