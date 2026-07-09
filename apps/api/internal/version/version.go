// Package version exposes the build version of the API binary.
//
// It is set via -ldflags "-X github.com/orpheus/api/internal/version.Version=..."
// at build time. The default is "dev" for local development.
package version

// Version is the semantic version of the API.
var Version = "0.1.0"
