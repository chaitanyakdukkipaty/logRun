package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:web/dist
var embeddedWebFS embed.FS

// webFileServer returns an http.Handler that serves the embedded React SPA.
// Requests to /api/* are NOT handled here — they go to the API mux.
// All other paths are served from web/dist; missing paths fall back to index.html
// so client-side routing works correctly.
func webFileServer() http.Handler {
	sub, err := fs.Sub(embeddedWebFS, "web/dist")
	if err != nil {
		panic("embedded web/dist not found: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try to serve the exact file first.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if _, err := fs.Stat(sub, path); err != nil {
			// File not found — serve index.html for SPA client-side routing.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
