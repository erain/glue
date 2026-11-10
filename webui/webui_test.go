// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package webui

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func checkSecurityHeaders(t *testing.T, h http.Header) {
	t.Helper()
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
		h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get("Referrer-Policy") != "no-referrer" ||
		h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("unexpected security headers: %v", h)
	}
}

func TestUIServesIndexWithSecurityHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d (%q)", w.Code, w.Header().Get("Content-Type"))
	}
	checkSecurityHeaders(t, w.Header())
	if !strings.Contains(w.Body.String(), "/static/app.js") {
		t.Fatalf("index.html missing /static/app.js: %s", w.Body.String())
	}
	t.Run("NoInlineScriptInIndex", func(t *testing.T) {
		assertNoInlineScript(t, w.Body.String())
	})
}

func TestStaticContentTypes(t *testing.T) {
	for _, tc := range []struct {
		path, wantType string
		wantStatus     int
	}{
		{"/static/app.js", "application/javascript", http.StatusOK},
		{"/static/app.css", "text/css", http.StatusOK},
		{"/static/missing.js", "text/plain", http.StatusNotFound},
		{"/static/../webui.go", "text/plain", http.StatusNotFound},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.wantStatus || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.wantType) {
				t.Fatalf("GET %s = %d (%q), want %d (%q)", tc.path, w.Code, w.Header().Get("Content-Type"), tc.wantStatus, tc.wantType)
			}
			checkSecurityHeaders(t, w.Header())
		})
	}
}

func TestNoInlineScriptInIndex(t *testing.T) {
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	assertNoInlineScript(t, string(data))
}

func assertNoInlineScript(t *testing.T, htmlDoc string) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(htmlDoc))
	dec.Strict, dec.AutoClose, dec.Entity = false, xml.HTMLAutoClose, xml.HTMLEntity
	var scripts int
	var inScript bool
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("parse HTML: %v", err)
		}
		switch v := tok.(type) {
		case xml.StartElement:
			if strings.EqualFold(v.Name.Local, "script") {
				scripts++
				inScript = true
				var src string
				for _, attr := range v.Attr {
					if strings.EqualFold(attr.Name.Local, "src") {
						src = strings.TrimSpace(attr.Value)
					}
				}
				if src == "" {
					t.Fatal("found <script> tag without src attribute")
				}
			}
		case xml.EndElement:
			if strings.EqualFold(v.Name.Local, "script") {
				inScript = false
			}
		case xml.CharData:
			if inScript && strings.TrimSpace(string(v)) != "" {
				t.Fatalf("found inline script content: %q", string(v))
			}
		}
	}
	if scripts == 0 {
		t.Fatal("expected at least one <script src=...> tag")
	}
}

func TestUIAssetsViewsAndSafety(t *testing.T) {
	jsBytes, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("app.js must not use %s for untrusted content", forbidden)
		}
	}
	for _, want := range []string{
		"/v1/session/login", "/v1/session", "X-Glue-CSRF", "REFRESH_MS = 10000", "TRUNCATE_LIMIT = 4096",
		"EventSource", "seenSeqs", "login-view", "jobs-view", "status-filter", "job-detail-view",
		"job-checklist", "job-transcript", "tool-show-more", "permission-card", "goal-divider",
		"iteration_start", "verdict", "operator_message", "run_interrupted", "job_resumed", "job_done",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("app.js missing %q", want)
		}
	}
	cssBytes, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBytes), "@media (max-width: 700px)") {
		t.Fatal("app.css missing @media (max-width: 700px) responsive breakpoint")
	}
}
