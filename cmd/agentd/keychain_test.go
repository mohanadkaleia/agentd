package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileSecretsAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTD_SECRETS_DIR", dir)
	if err := keychainStoreHex("agentd", "gmail-oauth", []byte("token")); err == nil {
		t.Fatal("stored a secret in a directory other users can open")
	}
	os.Chmod(dir, 0o700)
	if err := keychainStoreHex("agentd", "gmail-oauth", []byte("token")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "gmail-oauth"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if got, ok := keychainLoadHex("agentd", "gmail-oauth"); !ok || string(got) != "token" {
		t.Fatalf("loaded %q, %v", got, ok)
	}
	if _, err := secretFile("../escape"); err == nil {
		t.Fatal("accepted a path in a secret name")
	}
	if err := keychainDelete("agentd", "gmail-oauth"); err != nil {
		t.Fatal(err)
	}
	if _, ok := keychainLoadHex("agentd", "gmail-oauth"); ok {
		t.Fatal("secret survived delete")
	}
}
