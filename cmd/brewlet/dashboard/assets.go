// Package dashboard contains the dependency-free, embedded operator interface.
package dashboard

import "embed"

//go:embed index.html dashboard.css app.mjs model.mjs
var Assets embed.FS
