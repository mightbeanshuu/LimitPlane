//go:build !linux

package main

import (
	"fmt"
	"os"
)

// XDP is a Linux kernel feature. The sync logic in sync.go still builds and
// tests everywhere; only the agent itself needs Linux.
func main() {
	fmt.Fprintln(os.Stderr, "limitplane-xdp: XDP needs Linux; run this on the gateway's Linux host")
	os.Exit(1)
}
