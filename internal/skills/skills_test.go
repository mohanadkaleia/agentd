package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDirAndPrompt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "research.md"), []byte("Check two independent sources.\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o600)
	r := Registry{}
	if err := LoadDir(r, dir); err != nil {
		t.Fatal(err)
	}
	if err := LoadDir(r, filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("missing skills dir should be fine: %v", err)
	}
	loaded, err := r.Resolve([]string{"research"})
	if err != nil || len(r) != 1 {
		t.Fatalf("loaded=%v registry=%d err=%v", loaded, len(r), err)
	}
	if p := Prompt(loaded); !strings.Contains(p, "## research\nCheck two independent sources.") {
		t.Fatalf("prompt = %q", p)
	}
	if _, err := r.Resolve([]string{"nope"}); err == nil {
		t.Fatal("unknown skill resolved")
	}
}
