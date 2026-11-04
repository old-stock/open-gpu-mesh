// Package docs embeds the user documentation so the binary can serve it
// (for example as MCP resources).
package docs

import "embed"

// FS holds every markdown file in docs/.
//
//go:embed *.md
var FS embed.FS
