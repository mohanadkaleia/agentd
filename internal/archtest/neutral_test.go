package archtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	adapterPrefix = "agentd/internal/model/"
	mcpSDK        = "github.com/modelcontextprotocol/"
)

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
			if strings.HasPrefix(pkg, mcpSDK) && dir != "internal/integrations/mcp" && !strings.HasPrefix(dir, "internal/integrations/mcp/") {
				t.Errorf("%s imports the MCP SDK; only internal/integrations/mcp may", rel)
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

func TestOnlyGatewayRunsTools(t *testing.T) {
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
		if filepath.ToSlash(filepath.Dir(rel)) == "internal/gateway" {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			self := ""
			if fn.Recv != nil && len(fn.Recv.List[0].Names) > 0 {
				self = fn.Recv.List[0].Names[0].Name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Execute" && sel.Sel.Name != "Reconcile") {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == self {
					return true
				}
				t.Errorf("%s calls %s outside internal/gateway; side effects must go through the gateway", rel, sel.Sel.Name)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEngineDoesNotDependOnChat(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal/runtime", "internal/gateway", "internal/scheduler", "internal/policy", "internal/tasks"} {
		entries, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range entries {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				if pkg, _ := strconv.Unquote(imp.Path.Value); pkg == "agentd/internal/chat" {
					t.Errorf("%s imports internal/chat; the engine must not depend on the conversation layer", path)
				}
			}
		}
	}
}
