//go:build !ghostty

// gateway wraps libghostty's input encoders (internal/inputenc), so it only
// exists behind the ghostty tag. This stub keeps untagged `go build ./...`
// green and tells a stray runner what's missing.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "gateway requires -tags ghostty (libghostty-vt on PKG_CONFIG_PATH); see cmd/gateway/main.go")
	os.Exit(1)
}
