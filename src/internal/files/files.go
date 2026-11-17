// Package files resolves engine-reported relative paths to files inside a
// task directory. Checks run on the real (symlink-resolved) location at
// open time, never only on string prefixes.
package files

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	ErrOutside  = errors.New("path outside allowed root")
	ErrNotFile  = errors.New("not a regular file")
	ErrBadInput = errors.New("invalid relative path")
)

// cleanRel validates a slash- or backslash-separated relative path from the
// engine or a torrent and returns it in OS form.
func cleanRel(rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) || strings.Contains(rel, `\`) {
		return "", ErrBadInput
	}
	if strings.HasPrefix(rel, "/") || (len(rel) >= 2 && rel[1] == ':') {
		return "", ErrBadInput
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return "", ErrBadInput
		}
	}
	return filepath.FromSlash(rel), nil
}

func within(base, target string) bool {
	if target == base {
		return false
	}
	return strings.HasPrefix(target, base+string(filepath.Separator))
}

// realDir resolves dir and checks it lies strictly inside root.
func realDir(root, dir string) (string, error) { return resolveDir(root, dir, false) }

func resolveDir(root, dir string, allowRoot bool) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realD, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if !within(realRoot, realD) && !(allowRoot && realD == realRoot) {
		return "", ErrOutside
	}
	return realD, nil
}

// Open opens rel below taskDir (root itself or below it) for reading.
// Symlinks that lead anywhere outside taskDir are refused.
func Open(root, taskDir, rel string) (*os.File, fs.FileInfo, error) {
	r, err := cleanRel(rel)
	if err != nil {
		return nil, nil, err
	}
	realTask, err := resolveDir(root, taskDir, true)
	if err != nil {
		return nil, nil, err
	}
	full, err := filepath.EvalSymlinks(filepath.Join(realTask, r))
	if err != nil {
		return nil, nil, err
	}
	if !within(realTask, full) {
		return nil, nil, ErrOutside
	}
	f, err := os.OpenFile(full, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, ErrNotFile
	}
	// Guard against the path being swapped between resolve and open.
	if again, err := os.Lstat(full); err != nil || !os.SameFile(info, again) {
		f.Close()
		return nil, nil, ErrOutside
	}
	return f, info, nil
}

// Usage sums regular-file bytes under dir without following symlinks.
func Usage(dir string) (int64, int, error) {
	var total int64
	var count int
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
				count++
			}
		}
		return nil
	})
	return total, count, err
}

// RemoveTaskDir deletes a task directory after verifying it is a real
// directory (not a symlink) strictly inside root. Links inside are removed
// as links; their targets are untouched.
func RemoveTaskDir(root, taskDir string) error {
	info, err := os.Lstat(taskDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return ErrOutside
	}
	realTask, err := realDir(root, taskDir)
	if err != nil {
		return err
	}
	return os.RemoveAll(realTask)
}

// FreeBytes reports space available to unprivileged users at path.
func FreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
