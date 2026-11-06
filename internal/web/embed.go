package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html app.js outbox.js sw.js style.css manifest.webmanifest icon.svg icon-192.png icon-512.png
var assets embed.FS

func Handler() http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.URL.Path != "/" {
			name := r.URL.Path[1:]
			if _, err := fs.Stat(assets, name); err != nil {
				http.NotFound(w, r)
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
