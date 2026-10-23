//go:build linux

package procexec

import "os/exec"

// capNetRaw is the numeric value of CAP_NET_RAW — the same as
// golang.org/x/sys/unix.CAP_NET_RAW, inlined to avoid a direct dependency.
// It covers raw-socket and promiscuous-mode access needed by nmap, hping3 and
// tcpdump. CAP_NET_ADMIN is intentionally absent: it grants interface/routing
// modification which none of the diagnostic tools require and would let a
// compromised agent modify the pod's network namespace.
const capNetRaw = 13

// applyNetCaps raises CAP_NET_RAW as an ambient capability on the child
// command so that nmap, hping3 and tcpdump work under a non-root uid.
//
// The kernel silently ignores ambient raises for capabilities not already in
// the process's permitted and inheritable sets, so this is a no-op on any
// machine or pod that has not been granted CAP_NET_RAW — safe to call
// unconditionally.
func applyNetCaps(cmd *exec.Cmd) {
	cmd.SysProcAttr.AmbientCaps = []uintptr{capNetRaw}
}
