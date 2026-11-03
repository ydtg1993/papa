package main

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestRunNewNamespacedModuleBuilds(t *testing.T) {
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"example.com/demo", "example.com/team/my-crawler", "example.com/demo/v2"} {
		t.Run(module, func(t *testing.T) {
			t.Chdir(t.TempDir())
			t.Setenv("GOWORK", "off")
			if err := runNew(module, []string{"--replace", filepath.ToSlash(repo)}); err != nil {
				t.Fatal(err)
			}
			root := filepath.FromSlash(module)
			name := path.Base(module)
			entries, err := os.ReadDir(filepath.Join(root, "internal", "sites"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].IsDir() || entries[0].Name() != name+".go" {
				t.Fatalf("sites must contain only %s.go; got %v", name, entries)
			}
			if got := modulePath(readFile(t, filepath.Join(root, "go.mod"))); got != module {
				t.Fatalf("module=%q want %q", got, module)
			}
			app := readFile(t, filepath.Join(root, "internal", "app", "run.go"))
			if !strings.Contains(app, `"`+module+`/internal/sites"`) {
				t.Fatal("imports must retain the full module path")
			}
			site := readFile(t, filepath.Join(root, "internal", "sites", name+".go"))
			if !strings.Contains(site, `Key:     "`+name+`"`) {
				t.Fatalf("site key should be the project name: %s", site)
			}
			makefile := readFile(t, filepath.Join(root, "Makefile"))
			if !strings.Contains(makefile, "bin/"+name+"$(EXE)") || strings.Contains(makefile, "bin/"+module) {
				t.Fatal("binary path must use the project name")
			}
			var compose struct {
				Services map[string]struct {
					ContainerName string `yaml:"container_name"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, "deploy", "docker", "docker-compose.yml"))), &compose); err != nil {
				t.Fatal(err)
			}
			if len(compose.Services) != 1 || compose.Services[name].ContainerName != name {
				t.Fatalf("invalid compose project names: %+v", compose.Services)
			}
			conventions := readFile(t, filepath.Join(root, skillPath(t, "references/project-conventions.md")))
			wantSitePath := "internal/sites/" + name + ".go"
			if !strings.Contains(conventions, wantSitePath) {
				t.Fatalf("generated conventions must reference %s", wantSitePath)
			}
			upgradeConventions, err := skillContent("skill_project-conventions.md.tmpl", skillData(root))
			if err != nil || !strings.Contains(string(upgradeConventions), wantSitePath) {
				t.Fatalf("upgrade conventions lost project name: %v", err)
			}
			for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
				cmd := exec.Command("go", args...)
				cmd.Dir = root
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("go %v: %v\n%s", args, err, output)
				}
			}
			binary := name
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}
			cmd := exec.Command("go", "build", "-o", filepath.Join("bin", binary), ".")
			cmd.Dir = root
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build entry binary: %v\n%s", err, output)
			}
			if _, err := os.Stat(filepath.Join(root, "bin", binary)); err != nil {
				t.Fatalf("missing project binary: %v", err)
			}
			if err := runCheckCmd([]string{root}); err != nil {
				t.Fatalf("generated sites check: %v", err)
			}
		})
	}
}
