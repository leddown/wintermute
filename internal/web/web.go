// Package web serves the embedded browser UI. The assets are compiled into
// the binary so deploying the server is a single file copy.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var assets embed.FS

// Handler serves the UI at the root path.
func Handler() http.Handler {
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		// Only reachable if the embed directive and this path disagree,
		// which is a build-time mistake.
		panic("web: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
