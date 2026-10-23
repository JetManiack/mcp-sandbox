//go:build linux

package procexec

import "os/exec"

// CAP_NET_RAW and CAP_NET_ADMIN as numeric constants — the same values as
// golang.org/x/sys/unix, kept inline to avoid a direct dependency on that
// package from this package.
const (
	capNetAdmin = 12
	capNetRaw   = 13
)

// applyNetCaps raises CAP_NET_RAW and CAP_NET_ADMIN as ambient capabilities
// on the child command so that network diagnostic tools (nmap, hping3, tcpdump)
// work under a non-root uid.
//
// The kernel silently ignores ambient raises for capabilities that are not
// already in the process's permitted and inheritable sets, so this is a no-op
// on any machine or pod that has not been granted those caps — safe to call
// unconditionally.
func applyNetCaps(cmd *exec.Cmd) {
	cmd.SysProcAttr.AmbientCaps = []uintptr{capNetAdmin, capNetRaw}
}
