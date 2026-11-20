//go:build !(js && wasm)

// rmt-web only exists as WebAssembly: see packaging/web/build.sh.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "rmt-web runs in a browser: build it with packaging/web/build.sh")
	os.Exit(2)
}
