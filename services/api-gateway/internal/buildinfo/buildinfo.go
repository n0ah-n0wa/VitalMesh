// Package buildinfo exposes build-time metadata injected by the linker.
package buildinfo

// Version identifies the build (normally the git commit). It is overridden at
// link time by the Makefile:
//
//	-X github.com/n0ah-n0wa/VitalMesh/services/api-gateway/internal/buildinfo.Version=<git-sha>
var Version = "dev"
