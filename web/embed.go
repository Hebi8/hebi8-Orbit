package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embeddedDist embed.FS

// DistFS returns the embedded frontend dist filesystem rooted at web/dist.
func DistFS() (fs.FS, error) {
	return fs.Sub(embeddedDist, "dist")
}
