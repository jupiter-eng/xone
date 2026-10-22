package xutil

import (
	"os"
	"path/filepath"
)

// ExecutableName is the file name of the running program -- "order-api" for /usr/local/bin/order-api --
// and an empty string when the operating system cannot tell.
//
// It is what a component falls back on for a name nobody configured. OTel's SDK builds its own
// service.name fallback out of exactly this, as unknown_service:<name> (resource/builtin.go, SDK
// v1.46.0), so whoever falls back on it is naming the same program OTel would name.
//
// Measured on go1.25/darwin: a built binary gives its own file name, and `go run .` gives the name of
// the directory built, so neither is the anonymous value a missing name would otherwise leave behind.
func ExecutableName() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Base(path)
}
