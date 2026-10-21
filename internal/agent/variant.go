//go:build !nodlopen

package agent

// buildVariant is "" for the normal build and "static" for the fully static Linux build
// (built with -tags nodlopen) used on systems without glibc, e.g. Alpine.
const buildVariant = ""
