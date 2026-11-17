package app

import "os"

// ownerOf has nothing to compare on Windows: a file keeps its access list when it is written in place, and the
// tests show that the file is the same file.
func ownerOf(os.FileInfo) string { return "" }
