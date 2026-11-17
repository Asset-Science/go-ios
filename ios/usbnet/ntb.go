// Package usbnet shares the host's network with an iPhone over the USB CDC-NCM link the phone
// exposes in its network mode (USB "mode 3"). It is the user-mode host side of that link: NCM
// transfer blocks in and out, a one-lease DHCP server and gateway ARP for the phone, and a
// userspace TCP/IP stack that relays the phone's connections through ordinary host sockets.
//
// Measured on an iPhone 15 Pro Max, iOS 27 (Asset Science DESK-4038):
//   - the phone sends 32-bit NTBs ("ncmh") but only accepts 16-bit NTBs ("NCMH") from the host;
//   - it never sends the CDC NETWORK_CONNECTION notification (it sends a non-standard 0x2B), so a
//     host that waits for link-up — like Windows' inbox UsbNcm driver — never passes a frame;
//   - it needs the requests Linux's cdc_ncm sends at bring-up, then behaves as a DHCP client.
//
// Everything in this file and in link.go is pure and OS-neutral; the USB transport is in
// winusb_windows.go.
package usbnet

import (
	"encoding/binary"
	"errors"
)

const logModule = "go-ios/usbnet"

// NCM 1.0 class requests to the communication interface, and their bmRequestType.
const (
	requestTypeClassInterfaceOut = 0x21
	requestTypeClassInterfaceIn  = 0xA1

	requestSetEthernetPacketFilter = 0x43
	requestGetNtbParameters        = 0x80
	requestSetNtbInputSize         = 0x86

	ntbParametersLength = 28

	// SET_ETHERNET_PACKET_FILTER: directed | broadcast | all-multicast, what Linux's cdc_ncm sends.
	defaultPacketFilter = 1<<2 | 1<<3 | 1<<1
)

// NtbParameters is the GET_NTB_PARAMETERS reply (NCM 1.0 table 6-3).
type NtbParameters struct {
	FormatsSupported       uint16
	NtbInMaxSize           uint32
	NdpInDivisor           uint16
	NdpInPayloadRemainder  uint16
	NdpInAlignment         uint16
	NtbOutMaxSize          uint32
	NdpOutDivisor          uint16
	NdpOutPayloadRemainder uint16
	NdpOutAlignment        uint16
	NtbOutMaxDatagrams     uint16
}

var errMalformed = errors.New("usbnet: malformed transfer block")

// ParseNtbParameters decodes a GET_NTB_PARAMETERS reply.
func ParseNtbParameters(b []byte) (NtbParameters, error) {
	if len(b) < ntbParametersLength || int(binary.LittleEndian.Uint16(b)) < ntbParametersLength {
		return NtbParameters{}, errMalformed
	}
	le := binary.LittleEndian
	return NtbParameters{
		FormatsSupported:       le.Uint16(b[2:]),
		NtbInMaxSize:           le.Uint32(b[4:]),
		NdpInDivisor:           le.Uint16(b[8:]),
		NdpInPayloadRemainder:  le.Uint16(b[10:]),
		NdpInAlignment:         le.Uint16(b[12:]),
		NtbOutMaxSize:          le.Uint32(b[16:]),
		NdpOutDivisor:          le.Uint16(b[20:]),
		NdpOutPayloadRemainder: le.Uint16(b[22:]),
		NdpOutAlignment:        le.Uint16(b[24:]),
		NtbOutMaxDatagrams:     le.Uint16(b[26:]),
	}, nil
}

const (
	nth16Signature = 0x484D434E // "NCMH"
	ndp16NoCrc     = 0x304D434E // "NCM0"
	ndp16Crc       = 0x314D434E // "NCM1"
	nth16Length    = 12
	ndp16MinLength = 16

	nth32Signature = 0x686D636E // "ncmh"
	ndp32NoCrc     = 0x306D636E // "ncm0"
	ndp32Crc       = 0x316D636E // "ncm1"
	nth32Length    = 16
	ndp32MinLength = 32

	maxNdpsPerNtb = 64 // a longer NDP chain is treated as corrupt (and stops a self-loop)
)

// ParseNtb returns the Ethernet frames inside one NTB, 16- or 32-bit by its signature. The
// returned slices alias b. Any header, pointer or datagram outside the block is an error.
func ParseNtb(b []byte) ([][]byte, error) {
	if len(b) < 4 {
		return nil, errMalformed
	}
	if binary.LittleEndian.Uint32(b) == nth32Signature {
		return parseNtb32(b)
	}
	return parseNtb16(b)
}

func parseNtb16(b []byte) ([][]byte, error) {
	le := binary.LittleEndian
	if len(b) < nth16Length || le.Uint32(b) != nth16Signature || le.Uint16(b[4:]) != nth16Length {
		return nil, errMalformed
	}
	block := int(le.Uint16(b[8:]))
	if block < nth16Length || block > len(b) {
		return nil, errMalformed
	}
	var frames [][]byte
	ndp := int(le.Uint16(b[10:]))
	for hops := 0; ndp != 0; hops++ {
		if hops >= maxNdpsPerNtb || ndp < nth16Length || ndp+8 > block {
			return nil, errMalformed
		}
		sig, length := le.Uint32(b[ndp:]), int(le.Uint16(b[ndp+4:]))
		if (sig != ndp16NoCrc && sig != ndp16Crc) || length < ndp16MinLength || length%4 != 0 || ndp+length > block {
			return nil, errMalformed
		}
		terminated := false
		for e := 8; e+4 <= length; e += 4 {
			index, size := int(le.Uint16(b[ndp+e:])), int(le.Uint16(b[ndp+e+2:]))
			if index == 0 && size == 0 {
				terminated = true
				break
			}
			if index == 0 || size == 0 || index+size > block {
				return nil, errMalformed
			}
			frames = append(frames, b[index:index+size])
		}
		if !terminated {
			return nil, errMalformed
		}
		ndp = int(le.Uint16(b[ndp+6:]))
	}
	return frames, nil
}

func parseNtb32(b []byte) ([][]byte, error) {
	le := binary.LittleEndian
	if len(b) < nth32Length || le.Uint32(b) != nth32Signature || le.Uint16(b[4:]) != nth32Length {
		return nil, errMalformed
	}
	block := uint64(le.Uint32(b[8:]))
	if block < nth32Length || block > uint64(len(b)) {
		return nil, errMalformed
	}
	var frames [][]byte
	ndp := uint64(le.Uint32(b[12:]))
	for hops := 0; ndp != 0; hops++ {
		if hops >= maxNdpsPerNtb || ndp < nth32Length || ndp > block || block-ndp < 16 {
			return nil, errMalformed
		}
		sig, length := le.Uint32(b[ndp:]), uint64(le.Uint16(b[ndp+4:]))
		if (sig != ndp32NoCrc && sig != ndp32Crc) || length < ndp32MinLength || length%8 != 0 || length > block-ndp {
			return nil, errMalformed
		}
		terminated := false
		for e := uint64(16); e+8 <= length; e += 8 {
			index, size := uint64(le.Uint32(b[ndp+e:])), uint64(le.Uint32(b[ndp+e+4:]))
			if index == 0 && size == 0 {
				terminated = true
				break
			}
			if index == 0 || size == 0 || index > block || size > block-index {
				return nil, errMalformed
			}
			frames = append(frames, b[index:index+size])
		}
		if !terminated {
			return nil, errMalformed
		}
		ndp = uint64(le.Uint32(b[ndp+8:]))
	}
	return frames, nil
}

// BuildNtb16 packs frames into one 16-bit NTB (NTH16, one NDP16 "NCM0" right after it, then the
// datagrams, each at offset % divisor == remainder as the device asked). It returns nil if the
// block would exceed maxSize, maxDatagrams (0 = no limit) or the 16-bit format's 65535 bytes.
func BuildNtb16(sequence uint16, frames [][]byte, p NtbParameters) []byte {
	if len(frames) == 0 || (p.NtbOutMaxDatagrams != 0 && len(frames) > int(p.NtbOutMaxDatagrams)) {
		return nil
	}
	align := int(p.NdpOutDivisor)
	if align == 0 {
		align = 4
	}
	remainder := int(p.NdpOutPayloadRemainder) % align
	ndpLength := 8 + 4*(len(frames)+1)
	offsets := make([]int, len(frames))
	cursor := nth16Length + ndpLength
	for i, f := range frames {
		if len(f) == 0 {
			return nil
		}
		for cursor%align != remainder {
			cursor++
		}
		offsets[i] = cursor
		cursor += len(f)
	}
	if cursor > 0xFFFF || (p.NtbOutMaxSize != 0 && cursor > int(p.NtbOutMaxSize)) {
		return nil
	}
	out := make([]byte, cursor)
	le := binary.LittleEndian
	le.PutUint32(out, nth16Signature)
	le.PutUint16(out[4:], nth16Length)
	le.PutUint16(out[6:], sequence)
	le.PutUint16(out[8:], uint16(cursor))
	le.PutUint16(out[10:], nth16Length)
	le.PutUint32(out[12:], ndp16NoCrc)
	le.PutUint16(out[16:], uint16(ndpLength))
	for i, f := range frames {
		le.PutUint16(out[20+4*i:], uint16(offsets[i]))
		le.PutUint16(out[22+4*i:], uint16(len(f)))
		copy(out[offsets[i]:], f)
	}
	return out
}

// Bounds on the NTB input size the host asks for: one 16-bit NTB cannot exceed 64 KiB, so a larger
// read buffer would never be filled by a valid block; NCM 1.0 §6.2.7's 2048 is used only when the
// device reports no size at all.
const (
	minNtbInSize = 2048
	maxNtbInSize = 1 << 16
)

// HostNtbInSize is the SET_NTB_INPUT_SIZE value for a device advertising `advertised`: the
// device's own maximum (never more — the host may not ask beyond it), kept within what the host
// reads in one transfer. The same value sizes the host's read buffer, so the device is never told
// it may send more than the host can hold.
func HostNtbInSize(advertised uint32) uint32 {
	switch {
	case advertised == 0:
		return minNtbInSize
	case advertised > maxNtbInSize:
		return maxNtbInSize
	default:
		return advertised
	}
}
