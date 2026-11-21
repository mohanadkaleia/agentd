package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

func loadOrCreateToken(path string) (string, error) {
	if v := os.Getenv("AGENTD_API_TOKEN"); v != "" {
		if len(v) < 32 {
			return "", fmt.Errorf("AGENTD_API_TOKEN must be at least 32 characters")
		}
		return v, nil
	}
	body, err := os.ReadFile(path)
	if err == nil {
		info, statErr := os.Stat(path)
		if statErr == nil && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s is readable by other users; run chmod 600 on it", path)
		}
		token := strings.TrimSpace(string(body))
		if len(token) < 32 {
			return "", fmt.Errorf("%s holds a token shorter than 32 characters", path)
		}
		return token, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}
