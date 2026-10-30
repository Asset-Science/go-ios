//go:build windows

package usbnet

import (
	"net/netip"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// SystemDNSServer returns the first IPv4 DNS server of an adapter that is up and has a gateway,
// i.e. the resolver the station itself uses for the internet. Invalid if there is none.
func SystemDNSServer() netip.AddrPort {
	size := uint32(15 * 1024)
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, size)
		first := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, windows.GAA_FLAG_INCLUDE_GATEWAYS, 0, first, &size)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return netip.AddrPort{}
		}
		for a := first; a != nil; a = a.Next {
			if a.OperStatus != windows.IfOperStatusUp || a.FirstGatewayAddress == nil {
				continue
			}
			for d := a.FirstDnsServerAddress; d != nil; d = d.Next {
				sa, err := d.Address.Sockaddr.Sockaddr()
				if err != nil {
					continue
				}
				if v4, ok := sa.(*syscall.SockaddrInet4); ok {
					return netip.AddrPortFrom(netip.AddrFrom4(v4.Addr), 53)
				}
			}
		}
		return netip.AddrPort{}
	}
	return netip.AddrPort{}
}
