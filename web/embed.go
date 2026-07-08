// Package web embeds the TraceForge dashboard single-page app so the server can
// serve it straight from the binary (no external files at runtime).
package web

import "embed"

// FS holds the dashboard assets under static/.
//
//go:embed static
var FS embed.FS
