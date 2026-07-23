package webui

import "embed"

// Files contains the production dashboard assets. Keeping the embed directive
// beside the assets avoids a generated copy and makes the Go binary hermetic.
//
//go:embed index.html app.js styles.css favicon.svg
var Files embed.FS
