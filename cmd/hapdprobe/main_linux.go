//go:build linux

// hapdprobe is a small read-only diagnostic for the native hostapd control
// binding (internal/hostapd): it opens an interface control socket and runs a
// command (default STATUS), plus lists associated stations. It changes nothing
// on the radio. Usage: hapdprobe <ctrl-sock> [command]
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"wantasticmesh/internal/hostapd"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: hapdprobe <ctrl-sock> [command]")
		os.Exit(2)
	}
	sock := os.Args[1]
	cmd := "STATUS"
	if len(os.Args) > 2 {
		cmd = strings.Join(os.Args[2:], " ")
	}
	c, err := hostapd.Open(sock)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer c.Close()

	r, err := c.Request(cmd, 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	fmt.Printf("=== %s @ %s ===\n%s\n", cmd, sock, strings.TrimSpace(r))

	macs, err := c.Stations(2 * time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stations:", err)
		os.Exit(1)
	}
	fmt.Printf("=== associated stations: %d ===\n", len(macs))
	for _, m := range macs {
		fmt.Println(" ", m)
	}
	fmt.Println("HAPDPROBE-OK")
}
