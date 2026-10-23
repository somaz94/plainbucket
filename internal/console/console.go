// Package console serves the browser console: static files whose script signs S3
// requests in the page with the key the user types, so the server holds no session.
package console

import (
	"embed"
	"io/fs"
	"net/http"
)

// Prefix is the path the console is served under; no bucket name can start with "_".
const Prefix = "/_console/"

//go:embed static
var files embed.FS

// Handler serves the console's files under Prefix to GET and HEAD.
func Handler() http.Handler {
	static, _ := fs.Sub(files, "static")
	fileServer := http.StripPrefix(Prefix, http.FileServerFS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetSecurityHeaders(w.Header())
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ObjectSandbox is the Content-Security-Policy of every other response on a console
// host: it gives a stored page an opaque origin, so it cannot reach the console.
const ObjectSandbox = "sandbox allow-scripts allow-popups allow-forms"

// SetSecurityHeaders keeps the console's pages from being framed or opened by another
// page, from loading anything but their own files, and from leaking their URL.
func SetSecurityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; "+
		"connect-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-cache")
}
