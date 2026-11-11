// Package boundary enforces the one rule the whole dispatch.Dispatcher split exists for: a core
// package must not create an HTTP client, send a request directly, or read an API key from the
// environment. It must build a dispatch.Request and hand it to the host.
package boundary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostPackages do the things the core must not: they resolve a destination, hold a credential and
// send. Each entry is a module-relative directory prefix.
var hostPackages = []string{
	"proxy",
	"proxyapp",
	"dash",
	"cmd",
	"examples",
	"internal/modelinfo/litellm",
	"dispatch/dispatchtest",
}

// bannedSelectors are the client-side net/http identifiers that make a direct call. http.Header,
// http.MethodPost and the like are fine — the core builds requests, it just does not send them.
var bannedSelectors = map[string]bool{
	"Client":                true,
	"DefaultClient":         true,
	"Get":                   true,
	"Post":                  true,
	"PostForm":              true,
	"Head":                  true,
	"NewRequest":            true,
	"NewRequestWithContext": true,
}

func isHostPackage(relDir string) bool {
	for _, p := range hostPackages {
		if relDir == p || strings.HasPrefix(relDir, p+"/") {
			return true
		}
	}
	return false
}

func TestCoreNeverCallsOutDirectly(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var findings []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relDir := filepath.ToSlash(filepath.Dir(rel))
		if isHostPackage(relDir) {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		httpAlias, osAlias := importAliases(file)
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok {
					if pkg.Name == httpAlias && bannedSelectors[sel.Sel.Name] {
						findings = append(findings, fmt.Sprintf("%s:%d: %s.%s", rel,
							fset.Position(n.Pos()).Line, pkg.Name, sel.Sel.Name))
					}
				}
			}
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == osAlias &&
						(sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv") {
						if len(call.Args) > 0 {
							if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								v := strings.Trim(lit.Value, "\"`")
								if strings.Contains(v, "KEY") || strings.Contains(v, "TOKEN") {
									findings = append(findings, fmt.Sprintf("%s:%d: os.%s(%s)", rel,
										fset.Position(n.Pos()).Line, sel.Sel.Name, lit.Value))
								}
							}
						}
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) > 0 {
		t.Errorf("the core calls out directly or reads a credential; move this code to a host "+
			"package instead of widening the allow-list:\n%s", strings.Join(findings, "\n"))
	}
}

// importAliases returns the local names file uses for "net/http" and "os" (possibly aliased, or
// "" if not imported), so a selector check matches even a renamed import.
func importAliases(file *ast.File) (httpAlias, osAlias string) {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, "\"")
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch path {
		case "net/http":
			if name != "" {
				httpAlias = name
			} else {
				httpAlias = "http"
			}
		case "os":
			if name != "" {
				osAlias = name
			} else {
				osAlias = "os"
			}
		}
	}
	return
}
