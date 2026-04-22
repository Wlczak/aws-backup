// Package webassets exposes the built Svelte SPA as an embed.FS so the
// Go binary can serve it without a co-located directory. The `dist/`
// folder is a Vite build artifact; run `npm run build` (or `make build`)
// before `go build`, otherwise only the .gitkeep placeholder is embedded.
package webassets

import "embed"

//go:embed all:dist
var Dist embed.FS
