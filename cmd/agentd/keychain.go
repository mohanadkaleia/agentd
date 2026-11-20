package main

import (
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

func keychainSecret(service, account string) string {
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func keychainStoreHex(service, account string, secret []byte) error {
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", service, account, hex.EncodeToString(secret)))
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
