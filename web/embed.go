// Package web embeds the single-file management UI in the executable.
package web

import "embed"

//go:embed dist
var assets embed.FS

func Index() ([]byte, error) { return assets.ReadFile("dist/index.html") }
