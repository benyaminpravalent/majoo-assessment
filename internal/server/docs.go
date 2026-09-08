package server

import (
	"net/http"

	"github.com/bpsiregar/majoo-assessment/api"
	"github.com/go-chi/chi/v5"
)

// registerDocsRoutes serves the embedded OpenAPI document and a browser UI for
// it.
//
// The UI is a static page that loads Swagger UI from a CDN. That keeps ~2 MB of
// vendored JavaScript out of the repository and the container image, at the
// cost of needing internet access to render — which is why the raw spec is also
// served, and why the README tells an offline reviewer to open api/openapi.yaml
// directly.
func registerDocsRoutes(r chi.Router) {
	r.Get("/openapi.yaml", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(api.OpenAPISpec)
	})

	r.Get("/docs", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerUIPage))
	})
}

// swaggerUIPage is a minimal Swagger UI host page pointing at /openapi.yaml.
// Pinned to an exact version with Subresource Integrity omitted deliberately:
// the CDN URL is version-pinned, and adding SRI hashes that nobody re-verifies
// on upgrade is security theatre.
const swaggerUIPage = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>Majoo Blog API — reference</title>
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css" />
    <style>
      body { margin: 0; background: #fafafa; }
    </style>
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
    <script>
      window.onload = function () {
        window.ui = SwaggerUIBundle({
          url: "/openapi.yaml",
          dom_id: "#swagger-ui",
          deepLinking: true,
          displayRequestDuration: true,
          tryItOutEnabled: true
        });
      };
    </script>
  </body>
</html>
`
