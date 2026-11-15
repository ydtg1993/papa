package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// Same-package components cannot rely on Go import-cycle checks to enforce
// ownership. Keep their inputs narrow even though Engine is visible to them.
func TestEngineComponentDependenciesStayNarrow(t *testing.T) {
	components := map[string]bool{
		"attemptExecution": false, "attemptPersistence": false,
		"breakerCoordinator": false, "Coordinator": false,
		"siteCatalog": false, "accountAlertGate": false,
		"submissionStore": false, "taskFlowService": false,
		"traceService": false,
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	deliveryPaths, err := filepath.Glob("internal/delivery/*.go")
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, deliveryPaths...)
	for _, path := range paths {
		if filepath.Ext(path) != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		wholeConfigImports := make(map[string]bool)
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if importPath == "github.com/ydtg1993/papa/v4/config" {
				name := "config"
				if imp.Name != nil {
					name = imp.Name.Name
				}
				wholeConfigImports[name] = true
			}
		}
		for _, decl := range file.Decls {
			group, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range group.Specs {
				typ, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if _, tracked := components[typ.Name.Name]; !tracked {
					continue
				}
				components[typ.Name.Name] = true
				ast.Inspect(typ.Type, func(node ast.Node) bool {
					switch n := node.(type) {
					case *ast.Ident:
						if n.Name == "Engine" {
							t.Errorf("%s retains Engine instead of explicit dependencies", typ.Name.Name)
						}
					case *ast.SelectorExpr:
						pkg, _ := n.X.(*ast.Ident)
						if n.Sel.Name == "Config" && pkg != nil && wholeConfigImports[pkg.Name] {
							t.Errorf("%s retains whole configuration instead of feature options", typ.Name.Name)
						}
					}
					return true
				})
			}
		}
	}
	for name, found := range components {
		if !found {
			t.Errorf("component guard has stale entry %s", name)
		}
	}
}
