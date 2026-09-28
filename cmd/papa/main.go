package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

//go:embed templates/*
var tmplFS embed.FS

type scaffoldData struct {
	Module string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "new":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: papa new <project-name>")
			os.Exit(1)
		}
		if err := runNew(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  papa new <project-name>   生成一个新爬虫项目骨架")
}

func runNew(name string) error {
	if name == "" {
		return fmt.Errorf("project name is empty")
	}
	data := scaffoldData{Module: name}

	files := []struct {
		tmpl string
		dest string
	}{
		{"go.mod.tmpl", "go.mod"},
		{"main.go.tmpl", "main.go"},
		{"config.yaml.tmpl", filepath.Join("configs", "config.yaml")},
		{"fetcher.go.tmpl", filepath.Join("fetcher", "fetcher.go")},
		{"content.go.tmpl", filepath.Join("models", "content.go")},
	}

	for _, f := range files {
		if err := render(f.tmpl, filepath.Join(name, f.dest), data); err != nil {
			return err
		}
	}

	// logs 目录：日志由框架托管，这里只占位保证目录存在
	if err := os.MkdirAll(filepath.Join(name, "logs"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(name, "logs", ".gitkeep"), nil, 0o644); err != nil {
		return err
	}

	fmt.Printf("project %q generated.\n", name)
	fmt.Printf("next: cd %s && go mod tidy && go run .\n", name)
	return nil
}

func render(tmplName, dest string, data scaffoldData) error {
	tmpl, err := template.ParseFS(tmplFS, "templates/"+tmplName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}
