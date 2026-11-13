package shell

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/ydtg1993/papa/v4/admin/breakers"
	"github.com/ydtg1993/papa/v4/admin/logs"
	"github.com/ydtg1993/papa/v4/admin/monitoring"
	"github.com/ydtg1993/papa/v4/admin/queues"
	"github.com/ydtg1993/papa/v4/admin/reports"
	"github.com/ydtg1993/papa/v4/admin/settings"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
	"github.com/ydtg1993/papa/v4/admin/tasktrace"
	"github.com/ydtg1993/papa/v4/admin/tokenadmin"
)

//go:embed static views
var resources embed.FS

type featureSource struct {
	name  string
	files fs.FS
}

var assembledOnce sync.Once
var assembled *resourceFS

// Assets exposes immutable feature resources and deterministic page
// bundles. Missing embedded inputs are startup errors, never partial pages.
func Assets() fs.ReadFileFS {
	assembledOnce.Do(func() {
		sources := []featureSource{{"shell", resources}, {"monitoring", monitoring.Assets}, {"queues", queues.Assets}, {"breakers", breakers.Assets}, {"settings", settings.Assets}, {"tokenadmin", tokenadmin.Assets}, {"logs", logs.Assets}, {"reports", reports.Assets}, {"tasktrace", tasktrace.Assets}, {"taskflow", taskflow.Assets}}
		var err error
		assembled, err = assemble(sources)
		if err != nil {
			panic(fmt.Errorf("assemble monitor resources: %w", err))
		}
	})
	return assembled
}

func assemble(sources []featureSource) (*resourceFS, error) {
	out := newResourceFS()
	for _, source := range sources {
		err := fs.WalkDir(source.files, ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			b, err := fs.ReadFile(source.files, name)
			if err != nil {
				return err
			}
			return out.add("features/"+source.name+"/"+name, b)
		})
		if err != nil {
			return nil, fmt.Errorf("%s resources: %w", source.name, err)
		}
	}
	read := func(name string) ([]byte, error) { return out.ReadFile(name) }
	template, err := read("features/shell/views/template.html")
	if err != nil {
		return nil, err
	}
	page := string(template)
	for _, source := range sources {
		prefix := "features/" + source.name + "/views/"
		for name, b := range out.files {
			if strings.HasPrefix(name, prefix) {
				marker := "{{/* feature:" + source.name + "/" + strings.TrimPrefix(name, prefix) + " */}}"
				page = strings.ReplaceAll(page, marker, string(b))
			}
		}
	}
	if strings.Contains(page, "{{/* feature:") {
		return nil, fmt.Errorf("unresolved feature template fragment")
	}
	if err := out.add("template.html", []byte(page)); err != nil {
		return nil, err
	}
	runtime, err := read("features/shell/static/runtime.js")
	if err != nil {
		return nil, err
	}
	// Explicit aliases avoid accidental collisions between feature controller.js files.
	aliases := []struct{ owner, name string }{
		{"shell", "mo.css"}, {"shell", "layout.css"}, {"shell", "table-links.js"}, {"shell", "requests.js"}, {"shell", "extensions.js"}, {"shell", "appearance.js"}, {"shell", "navigation.js"},
		{"shell", "fonts/space-grotesk-latin.woff2"}, {"shell", "fonts/archivo-black-latin.woff2"},
		{"monitoring", "workbench.js"}, {"monitoring", "workbench.css"}, {"monitoring", "visual-state.js"}, {"monitoring", "visuals.js"}, {"monitoring", "visuals.css"},
		{"queues", "queue-governance.js"}, {"queues", "queue-governance.css"}, {"tasktrace", "trace.js"},
	}
	for _, alias := range aliases {
		b, err := read("features/" + alias.owner + "/static/" + alias.name)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(alias.name, ".js") {
			b = javascript(string(runtime), b)
		}
		if alias.name == "mo.css" {
			css := string(b)
			for _, source := range sources {
				prefix := "features/" + source.name + "/static/"
				for name, fragment := range out.files {
					if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".css") && name != "features/shell/static/mo.css" {
						css = strings.ReplaceAll(css, "/* feature-css:"+source.name+"/"+strings.TrimPrefix(name, prefix)+" */", string(fragment))
					}
				}
			}
			if strings.Contains(css, "/* feature-css:") {
				return nil, fmt.Errorf("unresolved feature stylesheet fragment")
			}
			b = []byte(css)
		}
		if err = out.add("static/"+alias.name, b); err != nil {
			return nil, err
		}
	}
	var bundle strings.Builder
	bundle.Write(runtime)
	bundle.WriteString("\n;\n")
	for _, name := range []string{"shell/ui", "shell/session", "shell/table-adapter", "shell/login-refresh", "shell/navigation-controller", "monitoring/controller", "queues/controller", "breakers/controller", "settings/controller", "tokenadmin/controller", "logs/controller", "shell/download", "reports/controller", "shell/theme", "shell/startup"} {
		owner, file, _ := strings.Cut(name, "/")
		b, err := read("features/" + owner + "/static/" + file + ".js")
		if err != nil {
			return nil, err
		}
		bundle.Write(b)
		bundle.WriteString("\n;\n")
	}
	if err = out.add("static/mo.js", []byte(bundle.String())); err != nil {
		return nil, err
	}
	// Feature-scoped resources are also addressable without ambiguous basenames.
	for _, source := range sources {
		prefix := "features/" + source.name + "/static/"
		entries := map[string][]byte{}
		for name, b := range out.files {
			if strings.HasPrefix(name, prefix) {
				entries[name] = b
			}
		}
		for name, b := range entries {
			if strings.HasSuffix(name, ".js") {
				b = javascript(string(runtime), b)
			}
			if err := out.add("static/features/"+source.name+"/"+strings.TrimPrefix(name, prefix), b); err != nil {
				return nil, err
			}
		}
	}
	out.seal()
	return out, nil
}
