package usbnet

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testPhoneMAC = [6]byte{0xAE, 0xDF, 0xA1, 0xE2, 0x4E, 0xFF}

// clientDHCP builds a client DHCP frame (0.0.0.0:68 -> 255.255.255.255:67) with the given options.
func clientDHCP(options []byte, ciaddr [4]byte) []byte {
	b := make([]byte, 236)
	b[0], b[1], b[2] = 1, 1, 6
	binary.BigEndian.PutUint32(b[4:], 0x3903F326)
	copy(b[12:], ciaddr[:])
	copy(b[28:], testPhoneMAC[:])
	b = binary.BigEndian.AppendUint32(b, dhcpCookie)
	b = append(append(b, options...), 255)
	ip := ipv4UDP(netip.IPv4Unspecified(), netip.AddrFrom4([4]byte{255, 255, 255, 255}), dhcpClient, dhcpServer, b)
	return ethernet([6]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, testPhoneMAC, etherTypeIPv4, ip)
}

func replyOption(frame []byte, code byte) []byte {
	opts := frame[ethernetLen+28+240:]
	for at := 0; at+1 < len(opts) && opts[at] != 255; {
		if opts[at] == 0 {
			at++
			continue
		}
		l := int(opts[at+1])
		if opts[at] == code {
			return opts[at+2 : at+2+l]
		}
		at += 2 + l
	}
	return nil
}

func TestDHCPOffersThenAcknowledgesTheLinksAddress(t *testing.T) {
	l := DefaultLease()
	discover, ok := parseDHCPRequest(clientDHCP([]byte{53, 1, dhcpDiscover}, [4]byte{}))
	require.True(t, ok)
	assert.Equal(t, uint32(0x3903F326), discover.xid)
	assert.Equal(t, testPhoneMAC, discover.clientMAC)
	assert.Equal(t, byte(dhcpOffer), dhcpReplyType(discover, l))

	offer := buildDHCPReply(discover, l, dhcpOffer)
	assert.Equal(t, uint16(0), internetChecksum(offer[ethernetLen:ethernetLen+20]), "IPv4 header checksum verifies")
	assert.Equal(t, []byte{192, 168, 77, 2}, offer[ethernetLen+28+16:ethernetLen+28+20], "yiaddr")
	assert.Equal(t, []byte{dhcpOffer}, replyOption(offer, 53))
	assert.Equal(t, []byte{192, 168, 77, 1}, replyOption(offer, 3), "router")
	assert.Equal(t, []byte{192, 168, 77, 1}, replyOption(offer, 6), "DNS")
	assert.Equal(t, []byte{255, 255, 255, 0}, replyOption(offer, 1))
	assert.Equal(t, []byte{0, 0, 0x0E, 0x10}, replyOption(offer, 51))

	request, ok := parseDHCPRequest(clientDHCP([]byte{53, 1, dhcpRequest, 50, 4, 192, 168, 77, 2, 54, 4, 192, 168, 77, 1}, [4]byte{}))
	require.True(t, ok)
	assert.Equal(t, byte(dhcpAck), dhcpReplyType(request, l))

	renew, ok := parseDHCPRequest(clientDHCP([]byte{53, 1, dhcpRequest}, [4]byte{192, 168, 77, 2}))
	require.True(t, ok)
	assert.Equal(t, byte(dhcpAck), dhcpReplyType(renew, l), "renewal names the address in ciaddr")
}

func TestDHCPRefusesOtherAddressesAndServers(t *testing.T) {
	l := DefaultLease()
	stale, ok := parseDHCPRequest(clientDHCP([]byte{53, 1, dhcpRequest, 50, 4, 10, 0, 0, 5}, [4]byte{}))
	require.True(t, ok)
	assert.Equal(t, byte(dhcpNak), dhcpReplyType(stale, l))
	nak := buildDHCPReply(stale, l, dhcpNak)
	assert.Equal(t, []byte{0, 0, 0, 0}, nak[ethernetLen+28+16:ethernetLen+28+20], "NAK carries no address")
	assert.Nil(t, replyOption(nak, 1))

	other, ok := parseDHCPRequest(clientDHCP([]byte{53, 1, dhcpRequest, 54, 4, 10, 0, 0, 1}, [4]byte{}))
	require.True(t, ok)
	assert.Equal(t, byte(0), dhcpReplyType(other, l), "the client chose another server")

	release, ok := parseDHCPRequest(clientDHCP([]byte{53, 1, 7}, [4]byte{}))
	require.True(t, ok)
	assert.Equal(t, byte(0), dhcpReplyType(release, l))
}

func TestDHCPIgnoresEverythingElse(t *testing.T) {
	good := clientDHCP([]byte{53, 1, dhcpDiscover}, [4]byte{})
	cases := map[string]func([]byte) []byte{
		"truncated":            func(b []byte) []byte { return b[:100] },
		"not IPv4":             func(b []byte) []byte { b[12], b[13] = 0x86, 0xDD; return b },
		"TCP":                  func(b []byte) []byte { b[ethernetLen+9] = 6; return b },
		"to the client port":   func(b []byte) []byte { b[ethernetLen+22], b[ethernetLen+23] = 0, 68; return b },
		"BOOTREPLY":            func(b []byte) []byte { b[ethernetLen+28] = 2; return b },
		"no cookie":            func(b []byte) []byte { b[ethernetLen+28+236] = 0; return b },
		"IP length past frame": func(b []byte) []byte { b[ethernetLen+2] = 0xFF; return b },
	}
	for name, mutate := range cases {
		_, ok := parseDHCPRequest(mutate(append([]byte(nil), good...)))
		assert.False(t, ok, name)
	}
	_, ok := parseDHCPRequest(clientDHCP(nil, [4]byte{}))
	assert.False(t, ok, "plain BOOTP has no message type")
	_, ok = parseDHCPRequest(clientDHCP([]byte{53, 1, dhcpDiscover, 50, 40}, [4]byte{}))
	assert.False(t, ok, "option runs past the message")
}

func arpRequest(senderIP, target [4]byte) []byte {
	a := make([]byte, 28)
	binary.BigEndian.PutUint16(a, 1)
	binary.BigEndian.PutUint16(a[2:], etherTypeIPv4)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:], 1)
	copy(a[8:], testPhoneMAC[:])
	copy(a[14:], senderIP[:])
	copy(a[24:], target[:])
	return ethernet([6]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, testPhoneMAC, etherTypeARP, a)
}

func TestARPAnswersOnlyForTheGateway(t *testing.T) {
	l := DefaultLease()
	reply := arpReplyFor(arpRequest([4]byte{192, 168, 77, 2}, [4]byte{192, 168, 77, 1}), l)
	require.Len(t, reply, 42)
	assert.Equal(t, testPhoneMAC[:], reply[:6], "unicast back to the phone")
	assert.Equal(t, uint16(2), binary.BigEndian.Uint16(reply[ethernetLen+6:]))
	assert.Equal(t, l.HostMAC[:], reply[ethernetLen+8:ethernetLen+14])
	assert.Equal(t, []byte{192, 168, 77, 1}, reply[ethernetLen+14:ethernetLen+18])
	assert.Equal(t, []byte{192, 168, 77, 2}, reply[ethernetLen+24:ethernetLen+28])

	assert.Nil(t, arpReplyFor(arpRequest([4]byte{}, [4]byte{192, 168, 77, 2}), l), "the phone's duplicate-address probe")
	assert.Nil(t, arpReplyFor(reply, l), "a reply is not a request")
	assert.Nil(t, arpReplyFor(arpRequest([4]byte{}, [4]byte{192, 168, 77, 1})[:41], l))
}

func TestInternetChecksumMatchesRFC1071(t *testing.T) {
	assert.Equal(t, uint16(0x220D), internetChecksum([]byte{0x00, 0x01, 0xF2, 0x03, 0xF4, 0xF5, 0xF6, 0xF7}))
	assert.Equal(t, ^uint16(0x0100), internetChecksum([]byte{0x01}))
}

func TestIPv4PayloadOnlyForIPv4(t *testing.T) {
	p := ipv4UDP(netip.AddrFrom4([4]byte{192, 168, 77, 2}), netip.AddrFrom4([4]byte{1, 1, 1, 1}), 5000, 53, []byte{1})
	assert.Equal(t, p, ipv4Payload(ethernet(DefaultLease().HostMAC, testPhoneMAC, etherTypeIPv4, p)))
	assert.Nil(t, ipv4Payload(ethernet(DefaultLease().HostMAC, testPhoneMAC, etherTypeARP, p)))
	assert.Nil(t, ipv4Payload([]byte{1, 2, 3}))
}
