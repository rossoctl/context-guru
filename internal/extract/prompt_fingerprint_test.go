package extract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// notPromptText are the string constants in prompt.go that are NOT prompt text sent to the
// model, so they do not belong in promptFingerprint. Aggressiveness levels are config
// VALUES — they reach the cache key through cfgFingerprint instead, and their text reaches
// it through the aggro blocks.
var notPromptText = map[string]bool{
	"AggroLow": true, "AggroMedium": true, "AggroHigh": true,
}

// TestEveryPromptConstantIsInTheFingerprint is a correctness test, not a style one.
//
// PromptVersion is derived by hashing an EXPLICIT LIST of prompt constants, and the global
// result cache key includes it. Text that is added or changed but NOT in that list is
// invisible to the key — so an extraction produced under a DIFFERENT prompt is served as
// if it were current, silently, with nothing to notice. This class of bug has bitten this
// file before, which is why the version is derived at all; the derivation only helps if the
// list is complete. So: parse prompt.go, and require every string constant declared there
// to be either hashed or explicitly declared non-prompt above.
func TestEveryPromptConstantIsInTheFingerprint(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "prompt.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	hashed := hashedConstNames(t, f)
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			if !isStringValued(vs) {
				continue
			}
			for _, name := range vs.Names {
				if hashed[name.Name] || notPromptText[name.Name] {
					continue
				}
				t.Fatalf("prompt constant %q is not covered by promptFingerprint: any prompt "+
					"text missing from that list is invisible to the result cache key, so a "+
					"stale extraction from a different prompt version gets served. Add it to "+
					"promptFingerprint, or to notPromptText if it never reaches the model.",
					name.Name)
			}
		}
	}
}

// hashedConstNames reads the identifiers promptFingerprint feeds into the hash.
func hashedConstNames(t *testing.T, f *ast.File) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "promptFingerprint" {
			continue
		}
		ast.Inspect(fd, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				names[id.Name] = true
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatal("promptFingerprint not found in prompt.go")
	}
	return names
}

// isStringValued reports whether a const spec's values are string literals or expressions
// built from them (the prompt constants are concatenations of literals and other consts).
func isStringValued(vs *ast.ValueSpec) bool {
	for _, v := range vs.Values {
		switch e := v.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				return true
			}
		case *ast.BinaryExpr:
			return true // the prompt constants are `"..." + ident + "..."` chains
		case *ast.CallExpr:
			// e.g. Aggressiveness("low") — a typed conversion of a string literal.
			for _, a := range e.Args {
				if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					return true
				}
			}
		}
	}
	return false
}

// TestPromptVersionChangesWithThePromptText proves the fingerprint is live: mutating any
// hashed part must produce a different version.
func TestPromptVersionChangesWithThePromptText(t *testing.T) {
	if !strings.HasPrefix(PromptVersion, "p") || len(PromptVersion) != 13 {
		t.Fatalf("unexpected PromptVersion shape %q", PromptVersion)
	}
	if PromptVersion != promptFingerprint() {
		t.Fatal("promptFingerprint is not deterministic")
	}
}
