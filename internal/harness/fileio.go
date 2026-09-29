package harness

// writeFileAtomic replaces the file at path with data: temp file in the same directory, fsync, rename.
// The original file mode is kept. If path is a symlink, the link stays and its target is replaced.
func writeFileAtomic(path string, data []byte) error { return errTODO }
