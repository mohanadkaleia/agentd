package skills

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func LoadDir(r Registry, dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		r.Add(Skill{Name: strings.TrimSuffix(e.Name(), ".md"), Instructions: strings.TrimSpace(string(body))})
	}
	return nil
}
