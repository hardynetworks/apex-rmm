//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "Apex RMM Desktop runs on Windows only. Use the web dashboard on other systems.")
	os.Exit(1)
}
