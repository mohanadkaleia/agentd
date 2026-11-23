package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var secretName = regexp.MustCompile(`^[a-z0-9-]+$`)

func secretsDir() string {
	return os.Getenv("AGENTD_SECRETS_DIR")
}

func secretFile(account string) (string, error) {
	dir := secretsDir()
	if !secretName.MatchString(account) {
		return "", fmt.Errorf("invalid secret name %q", account)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("secrets directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secrets directory %s must be a directory only its owner can open (chmod 700)", dir)
	}
	return filepath.Join(dir, account), nil
}

func keychainSecret(service, account string) string {
	if secretsDir() != "" {
		path, err := secretFile(account)
		if err != nil {
			return ""
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func keychainStoreHex(service, account string, secret []byte) error {
	if secretsDir() != "" {
		path, err := secretFile(account)
		if err != nil {
			return err
		}
		tmp := path + ".new"
		if err := os.WriteFile(tmp, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", service, account, hex.EncodeToString(secret)))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// keychainStore keeps a secret as typed. It reaches the keychain tool on its standard
// input, never on a command line other users could read.
func keychainStore(service, account, secret string) error {
	if strings.ContainsAny(secret, "\n\r") {
		return errors.New("a secret cannot span lines")
	}
	if secretsDir() != "" {
		path, err := secretFile(account)
		if err != nil {
			return err
		}
		tmp := path + ".new"
		if err := os.WriteFile(tmp, []byte(secret), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, path)
	}
	if !secretName.MatchString(account) {
		return fmt.Errorf("invalid secret name %q", account)
	}
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(secret)
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w \"%s\"\n", service, account, quoted))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func keychainLoadHex(service, account string) ([]byte, bool) {
	v := keychainSecret(service, account)
	if v == "" {
		return nil, false
	}
	b, err := hex.DecodeString(v)
	return b, err == nil
}

func keychainDelete(service, account string) error {
	if secretsDir() != "" {
		path, err := secretFile(account)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if out, err := exec.Command("/usr/bin/security", "delete-generic-password", "-s", service, "-a", account).CombinedOutput(); err != nil {
		return fmt.Errorf("keychain: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
