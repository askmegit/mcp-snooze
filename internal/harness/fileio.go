package harness

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
)

var errConfigChangedWhileWriting = errors.New("config changed while writing")

// writeFileAtomic replaces the file at path with data: temp file in the same directory, fsync, rename.
// The original file mode is kept. If path is a symlink, the link stays and its target is replaced.
func writeFileAtomic(path string, data []byte) error {
	return writeFileAtomicChecked(path, data, nil, false)
}

func writeFileAtomicExpected(path string, data, expect []byte) error {
	return writeFileAtomicChecked(path, data, expect, true)
}

func writeFileAtomicChecked(path string, data, expect []byte, check bool) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".mcp-snooze-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmpPath, info.Mode()); err != nil {
		return err
	}
	if check {
		current, err := os.ReadFile(target)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, expect) {
			return errConfigChangedWhileWriting
		}
	}
	return os.Rename(tmpPath, target)
}
