//go:build !windows

// The task center UI is a Windows desktop application; the macOS line ships its
// own native preview, so this build exists only to keep `go build ./...` green
// on every platform the module supports.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cloudfile-taskcenter-ui is a Windows-only application")
	os.Exit(2)
}
