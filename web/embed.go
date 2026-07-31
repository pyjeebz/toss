// Package web carries the frontend into the binary. One artifact, nothing to
// mount, nothing to serve separately.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html app.js app.css fonts
var files embed.FS

// FS is the static asset tree.
func FS() fs.FS { return files }
