package events

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

// allTypesFromSource parses this file rather than trusting All() to list what
// the constants declare. Go cannot enumerate constants at runtime, so the AST
// is the only independent source.
func allTypesFromSource(t *testing.T) []EventType {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate bus.go")
	}
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, filepath.Join(filepath.Dir(file), "bus.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse bus.go: %v", err)
	}

	var out []EventType
	for _, decl := range parsed.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if !isTypeConstant(name.Name) || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				out = append(out, EventType(lit.Value[1:len(lit.Value)-1]))
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no EventType constants found in bus.go; the parse is broken, not the source")
	}
	return out
}

func isTypeConstant(name string) bool {
	return len(name) > len("Type") && name[:len("Type")] == "Type"
}
