// Package web embeds the dashboard's static assets so the whole app ships as a
// single binary with no external file dependencies.
package web

import (
	"embed"
	"io/fs"
)

// IndexHTML holds the complete single-page dashboard (HTML + CSS + JS).
//
//go:embed index.html
var IndexHTML []byte

// vendorFS holds the third-party front-end libraries (xterm.js plus the fit and
// web-links addons). They are vendored into the binary rather than loaded from a
// CDN, so a deployment with no internet access still works.
//
//go:embed vendor
var vendorFS embed.FS

// Vendor serves the vendored assets with the "vendor" prefix stripped, so the
// URL /vendor/xterm.js maps to xterm.js in the embedded tree.
var Vendor fs.FS = mustSub(vendorFS, "vendor")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		// Unreachable: the directory is embedded at compile time.
		panic("web: embed vendor: " + err.Error())
	}
	return sub
}
