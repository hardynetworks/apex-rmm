//go:build !windows

package agent

import (
	"fmt"
	"os"
)

// PrintADL is Windows only: Linux reads AMD GPUs through amdgpu's sysfs files.
func PrintADL() int {
	fmt.Fprintln(os.Stderr, "adl: Windows only")
	return 1
}
