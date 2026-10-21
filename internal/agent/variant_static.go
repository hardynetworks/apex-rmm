//go:build nodlopen

package agent

// buildVariant: this is the fully static Linux build (no H.264 video, since OpenH264 can't be
// loaded without the system's C library). It updates itself to the static build too.
const buildVariant = "static"
