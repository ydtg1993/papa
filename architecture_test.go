package papa_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Boundaries constrain dependency direction, not file length or directory count.
func TestPackageResponsibilityBoundaries(t *testing.T) {
	const module = "github.com/ydtg1993/papa/v4/"
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		p := filepath.ToSlash(path)
		if strings.HasPrefix(p, "admin/server/") && !strings.Contains(p, "/testdata/") && (strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".html")) {
			t.Errorf("server composition owns feature/frontend source instead of its owning module: %s", p)
		}
		if strings.HasPrefix(p, "internal/app/") && (strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".html")) {
			t.Errorf("application composition owns frontend resource: %s", p)
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			dep, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(p, "config/") && dep == module+"internal/breaker" {
				t.Errorf("public config %s imports breaker implementation", p)
			}
			if strings.HasPrefix(p, "internal/tracestore/") && strings.Contains(strings.Split(dep, "/")[0], ".") && dep != module+"core" {
				t.Errorf("trace file store %s imports non-value dependency %s", p, dep)
			}
			if strings.HasPrefix(p, "pkg/account/gormstore/") && dep == module+"pkg/account" {
				t.Errorf("storage adapter %s imports account coordinator", p)
			}
			if strings.HasPrefix(p, "pkg/account/contract/") && strings.Contains(strings.Split(dep, "/")[0], ".") {
				t.Errorf("account value contract %s imports non-standard dependency %s", p, dep)
			}

			if strings.HasPrefix(p, "admin/") && dep == module+"internal/app" {
				t.Errorf("%s imports app", p)
			}
			if strings.HasPrefix(p, "internal/sitedecl/") && (dep == "net/http" || strings.HasPrefix(dep, module+"admin/") || dep == module+"internal/app") {
				t.Errorf("site declaration planner %s imports outer layer %s", p, dep)
			}
			if strings.HasPrefix(p, "admin/page/") && strings.HasPrefix(dep, module) {
				t.Errorf("page value contract %s imports framework implementation %s", p, dep)
			}
			if strings.HasPrefix(p, "admin/") && !strings.HasPrefix(p, "admin/server/") && dep == module+"admin/server" {
				t.Errorf("admin feature %s imports the server composition facade", p)
			}
			service := strings.HasPrefix(p, "internal/runtimecontrol/") || strings.HasPrefix(p, "internal/reportrunner/") || strings.HasPrefix(p, "internal/taskstore/")
			if strings.HasPrefix(p, "internal/reportrunner/") && dep == module+"config" {
				t.Errorf("report runner %s imports application configuration instead of its own value contract", p)
			}
			if service && (dep == "net/http" || strings.HasPrefix(dep, module+"admin/") || dep == module+"internal/app" || dep == module+"engine") {
				t.Errorf("service %s imports outer layer %s", p, dep)
			}
			if strings.HasPrefix(p, "engine/internal/") && (dep == module+"engine" || strings.HasPrefix(dep, module+"admin/") || dep == module+"internal/app") {
				t.Errorf("engine component %s imports outer layer %s", p, dep)
			}
			if strings.HasPrefix(p, "internal/browserdiagnostics/") && (strings.HasPrefix(dep, module+"admin/") || dep == module+"internal/app") {
				t.Errorf("diagnostic execution %s imports HTTP adapter %s", p, dep)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
