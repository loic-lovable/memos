// Package schemas embeds the draft wire contracts used by the CLI.
package schemas

import "embed"

// Files contains the repository's JSON Schema documents, without network loading.
//
//go:embed *.json
var Files embed.FS
