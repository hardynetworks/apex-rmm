//go:build !windows && !darwin && !(linux && (amd64 || arm64))

package desktop

import "errors"

const dlSupported = false

func dlOpen(string) (uintptr, error)         { return 0, errors.New("not supported on this platform") }
func dlSym(uintptr, string) (uintptr, error) { return 0, errors.New("not supported on this platform") }
func dlCall(uintptr, ...uintptr) uintptr     { return 0 }
