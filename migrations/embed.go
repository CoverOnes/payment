// Package migrations embeds all SQL migration files for single-binary deploy.
package migrations

import "embed"

// FS is the embedded migration file system.
//
//go:embed *.sql
var FS embed.FS
