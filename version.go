package main

// version is the server version. It is stamped at build time via
// -ldflags "-X main.version=<value>". The value is expected to be a
// MAJOR.MINOR string read from the VERSION file at the repo root.
//
// When the binary is built without ldflags (e.g. `go build ./...` or
// `go test`), version stays "dev" so the binary remains usable.
var version = "dev"
