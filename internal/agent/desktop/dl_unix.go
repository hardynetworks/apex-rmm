//go:build darwin || (linux && (amd64 || arm64) && !nodlopen)

package desktop

import "github.com/ebitengine/purego"

const dlSupported = true

func dlOpen(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func dlSym(lib uintptr, name string) (uintptr, error) { return purego.Dlsym(lib, name) }

func dlCall(fn uintptr, args ...uintptr) uintptr {
	r, _, _ := purego.SyscallN(fn, args...)
	return r
}
