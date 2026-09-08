// Package migrations embeds the SQL migration files into the binary.
//
// Embedding rather than reading from disk means `blogapi-migrate` and the API
// container are self-contained: there is no way to deploy an image whose code
// and migrations disagree, and no volume mount to get wrong in Kubernetes.
package migrations

import "embed"

// FS holds every .sql file in this directory.
//
//go:embed *.sql
var FS embed.FS
