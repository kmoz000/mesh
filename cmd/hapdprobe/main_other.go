//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "hapdprobe: linux-only (needs the hostapd control socket)")
	os.Exit(1)
}
