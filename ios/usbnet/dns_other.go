//go:build !windows

package usbnet

import "net/netip"

// SystemDNSServer is only needed by the Windows transport; elsewhere sharing is not implemented.
func SystemDNSServer() netip.AddrPort { return netip.AddrPort{} }
