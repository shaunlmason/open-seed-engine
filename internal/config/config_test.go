package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOperatorsAndLease(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"),
		[]byte("[coordination]\nbackend = \"filecards\"\n[operators]\nactors = [\"lead\"]\n[claim]\ndefault_lease = \"45m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsOperator("lead") || c.IsOperator("nobody") {
		t.Fatal("operator roster wrong")
	}
	if c.DefaultLease() != 45*time.Minute {
		t.Fatalf("lease: %v", c.DefaultLease())
	}
}

func TestLoadDefaultsAndErrors(t *testing.T) {
	// A missing config file falls back to defaults.
	c, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("missing config not defaulted: %v", err)
	}
	if c.DefaultLease() <= 0 {
		t.Fatal("no default lease")
	}
	// A malformed lease falls back too.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[claim]\ndefault_lease = \"not-a-duration\"\n"), 0o644)
	c, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultLease() <= 0 {
		t.Fatal("bad lease not defaulted")
	}
	// Unparseable TOML is an error.
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "config.toml"), []byte("[[[["), 0o644)
	if _, err := Load(dir2); err == nil {
		t.Fatal("bad toml accepted")
	}
}

func TestFindRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".seed"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, found := FindRoot(nested)
	if !found || got != root {
		t.Fatalf("FindRoot: %q %v", got, found)
	}
	if _, found := FindRoot(t.TempDir()); found {
		t.Fatal("root found where none exists")
	}
}
