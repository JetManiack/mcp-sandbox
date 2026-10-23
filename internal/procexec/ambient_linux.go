//go:build linux

package procexec

import "os/exec"

// applyNetCaps is a no-op: network tools in the image (ping, traceroute, dig,
// nc) use ICMP datagram sockets or TCP and do not require CAP_NET_RAW. The
// cluster sets net.ipv4.ping_group_range to allow ICMP datagram sockets for
// the agent uid range without any capabilities.
func applyNetCaps(_ *exec.Cmd) {}
