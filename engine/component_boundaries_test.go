package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Same-package components cannot rely on Go import-cycle checks to enforce
// ownership. Keep their inputs narrow even though Engine is visible to them.
func TestEngineComponentDependenciesStayNarrow(t *testing.T) {
	components := map[string]bool{
		"attemptExecution": false, "attemptPersistence": false,
		"breakerCoordinator": false, "spillQueue": false,
		"siteCatalog": false, "accountAlertGate": false,
		"submissionStore": false, "taskFlowService": false,
		"traceService": false, "archiveService": false,
	}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if filepath.Ext(path) != ".go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
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
						if n.Sel.Name == "Config" {
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
