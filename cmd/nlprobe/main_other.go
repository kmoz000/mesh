//go:build !linux

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "nlprobe: linux-only (needs nl80211 via libnl)")
	os.Exit(1)
}
