package main

import "runtime/debug"

// version is stamped by the release build (-ldflags "-X main.version=...").
// A plain `go build` leaves "dev".
var version = "dev"

// versionString is what `ariadne version` prints. A `go install ...@v0.4.0`
// build is not stamped by the linker but carries its module version in the
// build info, so that is used before falling back to "dev".
func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}
