// Package migrations embeds the SQL migrations applied on startup. They sit
// at the repo root so they're easy to find and read.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
