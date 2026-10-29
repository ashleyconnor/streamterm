// Package streamterm exists only to embed the web assets, which go:embed can
// reach only from the directory they live in.
package streamterm

import "embed"

// WebFS holds the browser-side assets. The `all:` prefix keeps files whose
// names begin with a dot or underscore, so web/vendor/.gitkeep survives and
// the build still succeeds before `make assets` has fetched terminal.css.
//
//go:embed all:web
var WebFS embed.FS
