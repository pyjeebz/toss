// Package web carries the frontend into the binary. One artifact, nothing to
// mount, nothing to serve separately.
package web

import (
	"embed"
	"io/fs"
)

// icons is listed file by file rather than as a directory so generate.sh, which
// is a development tool and not an asset, stays out of the binary.
//
//go:embed index.html app.js qr.js crypto.js app.css manifest.webmanifest
//go:embed fonts icons/icon.svg icons/*.png
var files embed.FS

// FS is the static asset tree.
func FS() fs.FS { return files }
