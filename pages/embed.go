// Package pages embeds the portal's SSR templates and static assets so the
// satellites binary ships self-contained — no external /app/pages COPY needed
// in the Dockerfile.
package pages

import (
	"embed"
	"html/template"
	"io/fs"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Templates parses all embedded HTML templates. Returns the resulting
// template set or an error. Callers should parse once at startup and cache.
func Templates() (*template.Template, error) {
	return template.ParseFS(templatesFS, "templates/*.html")
}

// Static returns the embedded filesystem rooted at "static/" so net/http can
// serve it under /static/.
func Static() (fs.FS, error) {
	return fs.Sub(staticFS, "static")
}
