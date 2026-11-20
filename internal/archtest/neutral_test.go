package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const adapterPrefix = "agentd/internal/model/"

var providerSDKs = []string{
	"github.com/anthropics/",
	"github.com/openai/",
	"github.com/ollama/",
	"google.golang.org/genai",
}

func TestOnlyAdaptersTouchProviders(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "bin") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.ToSlash(filepath.Dir(rel))
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			pkg, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(pkg, adapterPrefix) {
				adapter := strings.TrimPrefix(pkg, "agentd/")
				if dir != adapter && !strings.HasPrefix(dir, "cmd/") {
					t.Errorf("%s imports adapter %s; only cmd/ may choose a provider", rel, pkg)
				}
			}
			for _, sdk := range providerSDKs {
				if strings.HasPrefix(pkg, sdk) && !strings.HasPrefix(dir, "internal/model/") {
					t.Errorf("%s imports provider SDK %s outside an adapter package", rel, pkg)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
