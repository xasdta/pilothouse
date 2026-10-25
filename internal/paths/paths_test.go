package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// A symlink planted at the old predictable temp name (file.tmp) must not
// receive the secret, and the file ends up private.
func TestWriteFileAtomicIgnoresPlantedTemp(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "telegram.json")
	leak := filepath.Join(t.TempDir(), "leak")
	if err := os.Symlink(leak, file+".tmp"); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(file, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leak); !os.IsNotExist(err) {
		t.Fatalf("the planted symlink was followed: %v", err)
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "secret" {
		t.Fatalf("file = %q, %v", b, err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 { // the file and the planted link; no temp left over
		t.Fatalf("left behind: %v", ents)
	}
}
