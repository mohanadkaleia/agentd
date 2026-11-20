package ids

import (
	"crypto/rand"
	"encoding/hex"
)

func New(prefix string) string {
	b := make([]byte, 10)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}
