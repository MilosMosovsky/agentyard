package main

import (
	"bytes"
	"net/http"
	"testing"
)

// The page links these from <head>; they must come out of the binary as-is.
func TestFaviconRoutes(t *testing.T) {
	h, _ := demoHandler(t)
	for _, c := range []struct {
		path, ctype string
		body        []byte
	}{
		{"/favicon.svg", "image/svg+xml", faviconSVG},
		{"/favicon.png", "image/png", faviconPNG},
	} {
		rec := get(t, h, "GET", c.path, local)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != c.ctype {
			t.Errorf("%s: %d %q", c.path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if len(c.body) == 0 || !bytes.Equal(rec.Body.Bytes(), c.body) {
			t.Errorf("%s: body does not match the embedded asset", c.path)
		}
	}
	// Brand assets are behind the same private-network gate as everything else.
	if rec := get(t, h, "GET", "/favicon.svg", "8.8.8.8:1234"); rec.Code != http.StatusForbidden {
		t.Errorf("public viewer got %d", rec.Code)
	}
}
