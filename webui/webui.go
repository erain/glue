// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

// Package webui serves the embedded browser UI assets with strict security
// headers for glue serve and glue-gateway.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// CSP is the Content-Security-Policy header value applied to all UI responses.
const CSP = "default-src 'self'"

//go:embed static/*
var staticFS embed.FS

// Handler returns an http.Handler that serves "/" and "/static/*" from the
// embedded static directory with strict browser security headers.
func Handler() http.Handler {
	return http.HandlerFunc(serveUI)
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
}

func serveUI(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var assetPath, contentType string
	switch {
	case r.URL.Path == "/" || r.URL.Path == "/index.html":
		assetPath = "static/index.html"
		contentType = "text/html; charset=utf-8"
	case strings.HasPrefix(r.URL.Path, "/static/"):
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		switch path.Ext(name) {
		case ".js":
			contentType = "application/javascript; charset=utf-8"
		case ".css":
			contentType = "text/css; charset=utf-8"
		case ".html":
			contentType = "text/html; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		assetPath = "static/" + name
	default:
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(staticFS, assetPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
