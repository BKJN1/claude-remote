// Package web embeds the static front-end so the server is a single binary.
package web

import "embed"

//go:embed index.html login.html app.js style.css manifest.webmanifest icon-192.png icon-512.png vendor
var FS embed.FS
