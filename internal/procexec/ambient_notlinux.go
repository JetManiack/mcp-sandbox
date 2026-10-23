//go:build unix && !linux

package procexec

import "os/exec"

// applyNetCaps is a no-op on non-Linux unix platforms (macOS, BSDs).
// AmbientCaps is a Linux-only syscall interface.
func applyNetCaps(_ *exec.Cmd) {}
