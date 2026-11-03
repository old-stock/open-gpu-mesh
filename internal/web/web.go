// Package web embeds the ogm dashboard.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the dashboard single-page app.
func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	return http.FileServer(http.FS(sub))
}
