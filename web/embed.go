package web

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed index.html app.js session.js printing.js style.css
var files embed.FS

func Handler() http.Handler {
	static := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/sessions/") {
			data, _ := files.ReadFile("index.html")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.Method != "HEAD" {
				_, _ = w.Write(data)
			}
			return
		}
		static.ServeHTTP(w, r)
	})
}
