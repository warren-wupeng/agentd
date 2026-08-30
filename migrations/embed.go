// Package migrations embeds the SQL migration files so the single binary
// carries its own schema (`agentd-server migrate`).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
