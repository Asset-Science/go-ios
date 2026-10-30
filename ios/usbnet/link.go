package usbnet

import (
	"encoding/binary"
	"math/bits"
	"net/netip"
)

// The link's one lease. Each phone has its own point-to-point USB link, so there is no pool and no
// lease table: the phone always gets ClientAddr and the host is its gateway and DNS server.
type Lease struct {
	HostMAC    [6]byte
	HostAddr   netip.Addr
	ClientAddr netip.Addr
	Netmask    netip.Addr
	LeaseSecs  uint32
}

// DefaultLease is the addressing DESK-4014's macOS prototype and the DESK-4038 Windows bench used.
func DefaultLease() Lease {
	return Lease{
		HostMAC:    [6]byte{0x02, 0x44, 0x34, 0x30, 0x33, 0x38}, // locally administered
		HostAddr:   netip.AddrFrom4([4]byte{192, 168, 77, 1}),
		ClientAddr: netip.AddrFrom4([4]byte{192, 168, 77, 2}),
		Netmask:    netip.AddrFrom4([4]byte{255, 255, 255, 0}),
		LeaseSecs:  3600,
	}
}

// prefixLen is the lease's netmask as a prefix length; anything but a contiguous IPv4 mask
// between /8 and /30 falls back to /24, the only one ever handed out.
func (l Lease) prefixLen() int {
	if l.Netmask.Is4() {
		m := l.Netmask.As4()
		v := binary.BigEndian.Uint32(m[:])
		ones := bits.OnesCount32(v)
		if ones >= 8 && ones <= 30 && v == ^uint32(0)<<(32-ones) {
			return ones
		}
	}
	return 24
}

const (
	etherTypeIPv4 = 0x0800
	etherTypeARP  = 0x0806
	protoUDP      = 17
	dhcpServer    = 67
	dhcpClient    = 68
	dhcpCookie    = 0x63825363
	ethernetLen   = 14
)

// DHCP message types (RFC 2132 option 53).
const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6
)

type dhcpMessage struct {
	msgType   byte
	xid       uint32
	flags     uint16
	clientMAC [6]byte
	ciaddr    netip.Addr
	requested netip.Addr // option 50; invalid if absent
	server    netip.Addr // option 54; invalid if absent
}

// parseDHCPRequest decodes a client DHCP message carried in an Ethernet/IPv4/UDP frame to port
// 67; ok is false for anything else.
func parseDHCPRequest(frame []byte) (m dhcpMessage, ok bool) {
	const udpLen, bootpFixed = 8, 236
	if len(frame) < ethernetLen+20+udpLen+bootpFixed+4 || binary.BigEndian.Uint16(frame[12:]) != etherTypeIPv4 {
		return m, false
	}
	ip := frame[ethernetLen:]
	ihl := int(ip[0]&0x0F) * 4
	if ip[0]>>4 != 4 || ihl < 20 || ip[9] != protoUDP {
		return m, false
	}
	total := int(binary.BigEndian.Uint16(ip[2:]))
	if total < ihl+udpLen || total > len(ip) {
		return m, false
	}
	udp := ip[ihl:total]
	ulen := int(binary.BigEndian.Uint16(udp[4:]))
	if binary.BigEndian.Uint16(udp[2:]) != dhcpServer || ulen < udpLen+bootpFixed+4 || ulen > len(udp) {
		return m, false
	}
	b := udp[udpLen:ulen]
	if b[0] != 1 || b[1] != 1 || b[2] != 6 || binary.BigEndian.Uint32(b[bootpFixed:]) != dhcpCookie {
		return m, false
	}
	m.xid = binary.BigEndian.Uint32(b[4:])
	m.flags = binary.BigEndian.Uint16(b[10:])
	m.ciaddr = netip.AddrFrom4([4]byte(b[12:16]))
	copy(m.clientMAC[:], b[28:34])
	for at := bootpFixed + 4; at < len(b); {
		code := b[at]
		if code == 255 {
			break
		}
		if code == 0 {
			at++
			continue
		}
		if at+2 > len(b) || at+2+int(b[at+1]) > len(b) {
			return m, false
		}
		v := b[at+2 : at+2+int(b[at+1])]
		switch {
		case code == 53 && len(v) == 1:
			m.msgType = v[0]
		case code == 50 && len(v) == 4:
			m.requested = netip.AddrFrom4([4]byte(v))
		case code == 54 && len(v) == 4:
			m.server = netip.AddrFrom4([4]byte(v))
		}
		at += 2 + len(v)
	}
	return m, m.msgType != 0
}

// dhcpReplyType: OFFER to DISCOVER; ACK to a REQUEST for this link's address (option 50, or ciaddr
// when renewing), NAK to a REQUEST for another; 0 (no reply) when the client chose another server
// or for any other message.
func dhcpReplyType(m dhcpMessage, l Lease) byte {
	switch m.msgType {
	case dhcpDiscover:
		return dhcpOffer
	case dhcpRequest:
		if m.server.IsValid() && m.server != l.HostAddr {
			return 0
		}
		want := m.requested
		if !want.IsValid() {
			want = m.ciaddr
		}
		if want == l.ClientAddr {
			return dhcpAck
		}
		return dhcpNak
	}
	return 0
}

// buildDHCPReply returns the server's reply as an Ethernet frame, broadcast at both layers (the
// client has no address yet; RFC 2131 §4.1 allows it). UDP checksum 0 is legal on IPv4.
func buildDHCPReply(m dhcpMessage, l Lease, replyType byte) []byte {
	b := make([]byte, 236, 300)
	b[0], b[1], b[2] = 2, 1, 6
	binary.BigEndian.PutUint32(b[4:], m.xid)
	binary.BigEndian.PutUint16(b[10:], m.flags)
	if replyType != dhcpNak {
		c, h := l.ClientAddr.As4(), l.HostAddr.As4()
		copy(b[16:], c[:])
		copy(b[20:], h[:])
	}
	copy(b[28:], m.clientMAC[:])
	b = binary.BigEndian.AppendUint32(b, dhcpCookie)
	opt := func(code byte, v ...byte) { b = append(append(b, code, byte(len(v))), v...) }
	addr := func(code byte, a netip.Addr) { v := a.As4(); opt(code, v[:]...) }
	opt(53, replyType)
	addr(54, l.HostAddr)
	if replyType != dhcpNak {
		s := l.LeaseSecs
		opt(51, byte(s>>24), byte(s>>16), byte(s>>8), byte(s))
		addr(1, l.Netmask)
		addr(3, l.HostAddr)
		addr(6, l.HostAddr)
	}
	b = append(b, 255)
	for len(b) < 300 {
		b = append(b, 0) // the BOOTP minimum some clients still expect
	}
	broadcast := netip.AddrFrom4([4]byte{255, 255, 255, 255})
	ip := ipv4UDP(l.HostAddr, broadcast, dhcpServer, dhcpClient, b)
	return ethernet([6]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, l.HostMAC, etherTypeIPv4, ip)
}

// arpReplyFor answers an Ethernet/IPv4 ARP request for the host's address; nil otherwise.
func arpReplyFor(frame []byte, l Lease) []byte {
	if len(frame) < ethernetLen+28 || binary.BigEndian.Uint16(frame[12:]) != etherTypeARP {
		return nil
	}
	a := frame[ethernetLen:]
	be := binary.BigEndian
	if be.Uint16(a) != 1 || be.Uint16(a[2:]) != etherTypeIPv4 || a[4] != 6 || a[5] != 4 || be.Uint16(a[6:]) != 1 {
		return nil
	}
	if netip.AddrFrom4([4]byte(a[24:28])) != l.HostAddr {
		return nil
	}
	var senderMAC [6]byte
	copy(senderMAC[:], a[8:14])
	host := l.HostAddr.As4()
	r := make([]byte, 28)
	be.PutUint16(r, 1)
	be.PutUint16(r[2:], etherTypeIPv4)
	r[4], r[5] = 6, 4
	be.PutUint16(r[6:], 2) // reply
	copy(r[8:], l.HostMAC[:])
	copy(r[14:], host[:])
	copy(r[18:], senderMAC[:])
	copy(r[24:], a[14:18]) // the requester's address
	return ethernet(senderMAC, l.HostMAC, etherTypeARP, r)
}

// ipv4Payload returns the IPv4 packet inside an Ethernet frame (nil for anything else).
func ipv4Payload(frame []byte) []byte {
	if len(frame) < ethernetLen+20 || binary.BigEndian.Uint16(frame[12:]) != etherTypeIPv4 || frame[ethernetLen]>>4 != 4 {
		return nil
	}
	return frame[ethernetLen:]
}

func ethernet(dst, src [6]byte, etherType uint16, payload []byte) []byte {
	f := make([]byte, ethernetLen, ethernetLen+len(payload))
	copy(f, dst[:])
	copy(f[6:], src[:])
	binary.BigEndian.PutUint16(f[12:], etherType)
	return append(f, payload...)
}

// ipv4UDP builds an IPv4/UDP packet with a valid header checksum and UDP checksum 0.
func ipv4UDP(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	p := make([]byte, 28, 28+len(payload))
	be := binary.BigEndian
	p[0] = 0x45
	be.PutUint16(p[2:], uint16(28+len(payload)))
	p[8], p[9] = 64, protoUDP
	s, d := src.As4(), dst.As4()
	copy(p[12:], s[:])
	copy(p[16:], d[:])
	be.PutUint16(p[10:], internetChecksum(p[:20]))
	be.PutUint16(p[20:], sport)
	be.PutUint16(p[22:], dport)
	be.PutUint16(p[24:], uint16(8+len(payload)))
	return append(p, payload...)
}

// internetChecksum is the RFC 1071 one's-complement sum.
func internetChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xFFFF + sum>>16
	}
	return ^uint16(sum)
}
