package main

import (
	_ "embed"
	"net/http"
)

// Brand assets the running page actually serves. Marketing art (banners,
// app icons, social preview) stays out of the binary.
var (
	//go:embed assets/favicon.svg
	faviconSVG []byte
	//go:embed assets/favicon-32.png
	faviconPNG []byte
)

// staticAsset serves one embedded file with a day of caching.
func staticAsset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(body)
	}
}
