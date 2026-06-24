// Package web embeds the dashboard's static HTML so the whole app ships as a
// single binary with no external file dependencies.
package web

import _ "embed"

// IndexHTML holds the complete single-page dashboard (HTML + CSS + JS).
//
//go:embed index.html
var IndexHTML []byte
