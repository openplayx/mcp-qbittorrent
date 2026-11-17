package files

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "dest", "tsk_1")
	other := filepath.Join(root, "dest", "tsk_2")
	for _, d := range []string{filepath.Join(task, "Show"), other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(task, "Show", "a.mkv"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(other, "secret"), []byte("other task"), 0o644)
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("outside"), 0o644)
	os.Symlink(outside, filepath.Join(task, "Show", "link-out"))
	os.Symlink(filepath.Join(other, "secret"), filepath.Join(task, "Show", "link-other"))
	os.Symlink("a.mkv", filepath.Join(task, "Show", "link-in"))

	f, info, err := Open(root, task, "Show/a.mkv")
	if err != nil || info.Size() != 5 {
		t.Fatalf("open: %v", err)
	}
	f.Close()
	for _, rel := range []string{"../tsk_2/secret", "/etc/passwd", "Show/../../tsk_2/secret", `Show\a.mkv`, "Show/link-out", "Show/link-other", "", "Show//a.mkv"} {
		if f, _, err := Open(root, task, rel); err == nil {
			f.Close()
			t.Errorf("%q opened", rel)
		}
	}
	// A symlink staying within the task resolves fine.
	if f, _, err := Open(root, task, "Show/link-in"); err != nil {
		t.Errorf("internal link: %v", err)
	} else {
		f.Close()
	}
	// Task dir itself must be inside root.
	if _, _, err := Open(task, root, "dest/tsk_2/secret"); !errors.Is(err, ErrOutside) {
		t.Errorf("root escape: %v", err)
	}
}

func TestRemoveTaskDir(t *testing.T) {
	root := t.TempDir()
	task := filepath.Join(root, "dest", "tsk_1")
	os.MkdirAll(task, 0o755)
	keep := filepath.Join(t.TempDir(), "keep")
	os.WriteFile(keep, []byte("x"), 0o644)
	os.Symlink(keep, filepath.Join(task, "link"))
	os.WriteFile(filepath.Join(task, "data"), []byte("x"), 0o644)
	if err := RemoveTaskDir(root, root); err == nil {
		t.Fatal("removing root allowed")
	}
	evil := filepath.Join(root, "dest", "evil")
	os.Symlink(t.TempDir(), evil)
	if err := RemoveTaskDir(root, evil); err == nil {
		t.Fatal("symlinked task dir removed")
	}
	if err := RemoveTaskDir(root, task); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(task); !os.IsNotExist(err) {
		t.Fatal("task dir still exists")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("symlink target was deleted")
	}
}
