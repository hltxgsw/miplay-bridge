package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesConsole(t *testing.T) {
	handler := Handler()
	for _, path := range []string{"/", "/index.html"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, recorder.Code)
		}
		if contentType := recorder.Header().Get("Content-Type"); contentType != "text/html; charset=utf-8" {
			t.Fatalf("%s content type = %q", path, contentType)
		}
		if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("%s cache control = %q", path, cacheControl)
		}
		body := recorder.Body.String()
		for _, expected := range []string{
			"妙播桥",
			"清除妙播连接",
			"清除 DLNA 连接",
			"测试声卡",
			"/api/status",
			"/api/miplay/disconnect",
			"/api/dlna/disconnect",
			"/api/test-play",
		} {
			if !strings.Contains(body, expected) {
				t.Fatalf("%s body missing %q", path, expected)
			}
		}
	}
}

func TestHandlerRejectsUnknownPaths(t *testing.T) {
	handler := Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/nope", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestEmbeddedPageCarriesConsoleWiring(t *testing.T) {
	// The embedded page must keep the token plumbing the API guard expects
	// and stay free of external resources (offline LAN requirement).
	page := string(indexHTML)
	for _, expected := range []string{"x-bridge-token", "localStorage", "miplay-bridge-token"} {
		if !strings.Contains(page, expected) {
			t.Fatalf("index.html missing %q", expected)
		}
	}
	for _, forbidden := range []string{"http://cdn", "https://cdn", "unpkg.com", "jsdelivr"} {
		if strings.Contains(page, forbidden) {
			t.Fatalf("index.html must not reference the external host %q", forbidden)
		}
	}
}

func TestEmbeddedPageIsMobileFirst(t *testing.T) {
	// The owner casts from a phone, so the console must stay a single
	// no-build page that fits a phone viewport.
	page := string(indexHTML)
	for _, expected := range []string{
		`name="viewport"`,
		"width=device-width",
		"viewport-fit=cover",
		"env(safe-area-inset-bottom)",
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("index.html missing %q", expected)
		}
	}
	for _, forbidden := range []string{"react", "vue", "npm run", "webpack", "vite"} {
		if strings.Contains(strings.ToLower(page), forbidden) {
			t.Fatalf("index.html must stay dependency-free, found %q", forbidden)
		}
	}
}

func TestEmbeddedPageStreamsStatus(t *testing.T) {
	// The console subscribes to /api/events (fetch + stream reader, because
	// EventSource cannot attach the token header) and must keep a polling
	// fallback: if the stream dies on a sleeping phone the page still updates.
	page := string(indexHTML)
	for _, expected := range []string{"/api/events", "/api/status", "getReader", "AbortController", "setInterval"} {
		if !strings.Contains(page, expected) {
			t.Fatalf("index.html missing %q", expected)
		}
	}
}

func TestEmbeddedPageMarksEstimatedPlayhead(t *testing.T) {
	// MiPlay never reports the phone's playback position (the protocol only
	// lets the phone query us), so the playhead we show is a local estimate.
	// That must stay visible next to the clock -- an unlabelled seconds counter
	// reads as if the phone had told us, which is exactly the wrong impression.
	page := string(indexHTML)
	for _, expected := range []string{
		`id="now-est"`,
		"本机估算",
		`"local_estimate"`,
	} {
		if !strings.Contains(page, expected) {
			t.Fatalf("index.html missing %q (the estimate badge must not vanish)", expected)
		}
	}
}
