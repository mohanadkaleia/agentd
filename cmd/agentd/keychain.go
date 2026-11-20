package main

import (
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
