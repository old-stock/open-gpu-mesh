package server

import (
	"html"
	"net/http"
	"strings"
	"sync"

	"github.com/old-stock/open-gpu-mesh/internal/api"
	"github.com/old-stock/open-gpu-mesh/internal/apidocs"
)

// Regenerate docs/api-surface.md from internal/apidocs/openapi.yaml.
//go:generate go test -run TestDocsAPISurfaceUpToDate -update .

// scalarConfig configures the Scalar API reference (https://github.com/scalar/scalar).
const scalarConfig = `{
  "theme": "default",
  "darkMode": true,
  "layout": "modern",
  "persistAuth": true,
  "showToolbar": "never",
  "hideDownloadButton": false,
  "defaultHttpClient": {"targetKey": "shell", "clientKey": "curl"},
  "authentication": {"preferredSecurityScheme": "bearerAuth"},
  "metaData": {"title": "open-gpu-mesh API reference", "description": "REST API of the open-gpu-mesh hub"}
}`

const docsPage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>open-gpu-mesh API reference</title>
  <meta name="description" content="REST API reference of the open-gpu-mesh hub: container groups, queues, gateway, storage, nodes.">
  <link rel="alternate" type="application/json" title="OpenAPI" href="/openapi.json">
  <style>body { margin: 0; background: #0f0f0f; }</style>
</head>
<body>
  <noscript>
    <p style="color:#ddd;font-family:sans-serif;padding:2rem">This reference needs JavaScript.
    The raw API description is at <a href="/openapi.json">/openapi.json</a>
    (<a href="/openapi.yaml">YAML</a>), and a plain-text index at <a href="/llms.txt">/llms.txt</a>.</p>
  </noscript>
  <script id="api-reference" data-url="/openapi.json" data-configuration="` + "{{CONFIG}}" + `"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>
`

// registerDocRoutes serves the API reference. All routes are public.
//
//	GET /docs          Scalar API reference
//	GET /reference     redirect to /docs
//	GET /openapi.json  OpenAPI 3.1 document (version and server URL filled in)
//	GET /openapi.yaml  the same, as YAML
//	GET /llms.txt      plain-text operation index (llms.txt convention)
func (s *Server) registerDocRoutes(mux *http.ServeMux) {
	type rendered struct {
		json, yaml, llms []byte
		err              error
	}
	var (
		once sync.Once
		r    rendered
	)
	get := func() rendered {
		once.Do(func() {
			r.yaml = apidocs.YAML(api.Version, s.cfg.PublicURL)
			if r.json, r.err = apidocs.JSON(api.Version, s.cfg.PublicURL); r.err != nil {
				return
			}
			r.llms, r.err = apidocs.LLMsTxt(api.Version, s.cfg.PublicURL)
		})
		return r
	}
	serve := func(contentType string, pick func(rendered) []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			d := get()
			if d.err != nil {
				problem(w, http.StatusInternalServerError, "internal error", "api docs: "+d.err.Error())
				return
			}
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(pick(d))
		}
	}
	page := []byte(strings.Replace(docsPage, "{{CONFIG}}", html.EscapeString(scalarConfig), 1))

	mux.HandleFunc("GET /openapi.json", serve("application/json", func(d rendered) []byte { return d.json }))
	mux.HandleFunc("GET /openapi.yaml", serve("application/yaml; charset=utf-8", func(d rendered) []byte { return d.yaml }))
	mux.HandleFunc("GET /llms.txt", serve("text/plain; charset=utf-8", func(d rendered) []byte { return d.llms }))
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
	mux.HandleFunc("GET /reference", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs", http.StatusMovedPermanently)
	})
}
