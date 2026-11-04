// Package examples embeds the example deploy files so `ogm template` can list and deploy
// them without a checkout.
package examples

import "embed"

// FS holds the example YAML files.
//
//go:embed *.yaml
var FS embed.FS
