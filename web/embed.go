// Package webassets embeds the HTML templates and static files.
package webassets

import "embed"

//go:embed templates static
var FS embed.FS
