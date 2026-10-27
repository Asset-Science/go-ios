package usbnet

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phoneNtb32 is the first 96 bytes, verbatim, of a 256-byte block an iPhone 15 Pro Max (iOS 27)
// sent (DESK-4038 bench); the rest of the block is zero-filled. NTH32 "ncmh", NDP32 "ncm0" at 16
// listing two datagrams: an MLD report from the phone's MAC, then a second frame.
func phoneNtb32() []byte {
	b := []byte{
		0x6E, 0x63, 0x6D, 0x68, 0x10, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x10, 0x00, 0x00, 0x00,
		0x6E, 0x63, 0x6D, 0x30, 0x28, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x3A, 0x00, 0x00, 0x00, 0x6E, 0x00, 0x00, 0x00, 0xAA, 0x00, 0x00, 0x00, 0x56, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x33, 0x33, 0x00, 0x00, 0x00, 0x16,
		0xAE, 0xDF, 0xA1, 0xE2, 0x4E, 0xFF, 0x86, 0xDD, 0x60, 0x00, 0x00, 0x00, 0x00, 0x38, 0x00, 0x01,
		0xFE, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04, 0x2D, 0x42, 0x6A, 0xA3, 0xAF, 0x8F, 0x2E,
	}
	return append(b, make([]byte, 256-len(b))...)
}

// phoneParams are the limits the phone reported to GET_NTB_PARAMETERS.
var phoneParams = NtbParameters{FormatsSupported: 3, NtbInMaxSize: 32764, NdpInDivisor: 4, NdpInAlignment: 4,
	NtbOutMaxSize: 32764, NdpOutDivisor: 4, NdpOutAlignment: 4, NtbOutMaxDatagrams: 512}

func TestParsesThePhonesOwn32BitNtb(t *testing.T) {
	block := phoneNtb32()
	frames, err := ParseNtb(block)
	require.NoError(t, err)
	require.Len(t, frames, 2)
	assert.Len(t, frames[0], 0x6E)
	assert.Len(t, frames[1], 0x56)
	assert.Equal(t, []byte{0xAE, 0xDF, 0xA1, 0xE2, 0x4E, 0xFF}, frames[0][6:12], "source is the phone's MAC")
	assert.Equal(t, uint16(0x86DD), binary.BigEndian.Uint16(frames[0][12:]))

	_, err = ParseNtb(block[:255]) // dwBlockLength (256) exceeds what was read
	assert.Error(t, err)
}

func TestNtb16RoundTripsAndHonoursTheDevicesLimits(t *testing.T) {
	frames := [][]byte{fill(42, 1), fill(342, 2), fill(61, 3)}
	block := BuildNtb16(9, frames, phoneParams)
	require.NotNil(t, block)
	assert.Equal(t, uint32(nth16Signature), binary.LittleEndian.Uint32(block))
	assert.Equal(t, uint16(9), binary.LittleEndian.Uint16(block[6:]))
	got, err := ParseNtb(block)
	require.NoError(t, err)
	require.Len(t, got, len(frames))
	for i := range frames {
		assert.Equal(t, frames[i], got[i])
	}

	p := phoneParams
	p.NdpOutDivisor, p.NdpOutPayloadRemainder = 8, 2
	one, err := ParseNtb(BuildNtb16(0, [][]byte{fill(10, 1)}, p))
	require.NoError(t, err)
	require.Len(t, one, 1)

	p = phoneParams
	p.NtbOutMaxSize = 300
	assert.Nil(t, BuildNtb16(0, [][]byte{fill(342, 1)}, p), "over dwNtbOutMaxSize")
	p = phoneParams
	p.NtbOutMaxDatagrams = 1
	assert.Nil(t, BuildNtb16(0, [][]byte{fill(1, 1), fill(1, 2)}, p), "over max datagrams")
	assert.Nil(t, BuildNtb16(0, nil, phoneParams))
	assert.Nil(t, BuildNtb16(0, [][]byte{{}}, phoneParams), "empty frame")
	p = phoneParams
	p.NtbOutMaxSize = 0 // no device limit: the 16-bit format's own limit still applies
	assert.Nil(t, BuildNtb16(0, [][]byte{fill(70000, 1)}, p))
}

func TestBuildNtb16AlignsDatagrams(t *testing.T) {
	p := phoneParams
	p.NdpOutDivisor, p.NdpOutPayloadRemainder = 8, 2
	block := BuildNtb16(0, [][]byte{fill(10, 1)}, p)
	require.NotNil(t, block)
	offset := binary.LittleEndian.Uint16(block[20:]) // first datagram index in the NDP16
	assert.Equal(t, uint16(2), offset%8)
}

func TestRejectsMalformedNtbs(t *testing.T) {
	good := BuildNtb16(1, [][]byte{fill(60, 1)}, phoneParams)
	_, err := ParseNtb(good)
	require.NoError(t, err)

	cases := map[string]func([]byte){
		"bad signature":       func(b []byte) { b[0] = 'X' },
		"bad header length":   func(b []byte) { b[4] = 16 },
		"NDP past the block":  func(b []byte) { b[10] = 0xF0 },
		"NDP inside the NTH":  func(b []byte) { b[10] = 4 },
		"datagram past block": func(b []byte) { b[22] = 0xFF },
		"zero index":          func(b []byte) { b[20], b[21] = 0, 0 },
		"NDP points to itself": func(b []byte) {
			binary.LittleEndian.PutUint16(b[18:], nth16Length)
		},
	}
	for name, corrupt := range cases {
		b := append([]byte(nil), good...)
		corrupt(b)
		_, err := ParseNtb(b)
		assert.Error(t, err, name)
	}
	_, err = ParseNtb([]byte{1, 2, 3})
	assert.Error(t, err)

	two := BuildNtb16(1, [][]byte{fill(60, 1), fill(60, 2)}, phoneParams)
	binary.LittleEndian.PutUint16(two[16:], 16) // wLength cut so the zero pair is outside the NDP
	_, err = ParseNtb(two)
	assert.Error(t, err, "unterminated NDP")

	n32 := phoneNtb32()
	binary.LittleEndian.PutUint32(n32[24:], 16) // NDP32 next = itself
	_, err = ParseNtb(n32)
	assert.Error(t, err, "NDP32 loop")
	n32 = phoneNtb32()
	binary.LittleEndian.PutUint32(n32[36:], 0xFFFF) // datagram length past the block
	_, err = ParseNtb(n32)
	assert.Error(t, err)
}

func TestParsesNtbParameters(t *testing.T) {
	b := make([]byte, 28)
	le := binary.LittleEndian
	le.PutUint16(b, 28)
	le.PutUint16(b[2:], 3)
	le.PutUint32(b[4:], 32764)
	le.PutUint32(b[16:], 32764)
	le.PutUint16(b[20:], 4)
	le.PutUint16(b[26:], 512)
	p, err := ParseNtbParameters(b)
	require.NoError(t, err)
	assert.Equal(t, uint16(3), p.FormatsSupported)
	assert.Equal(t, uint32(32764), p.NtbInMaxSize)
	assert.Equal(t, uint16(4), p.NdpOutDivisor)
	assert.Equal(t, uint16(512), p.NtbOutMaxDatagrams)

	_, err = ParseNtbParameters(b[:27])
	assert.Error(t, err)
	le.PutUint16(b, 27)
	_, err = ParseNtbParameters(b)
	assert.Error(t, err)
}

func fill(n int, v byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = v
	}
	return b
}

func TestHostNtbInSizeStaysWithinWhatTheHostReads(t *testing.T) {
	for _, c := range []struct{ advertised, want uint32 }{
		{32764, 32764}, // the measured iPhone 15 Pro Max
		{0, minNtbInSize},
		{100, minNtbInSize},
		{maxNtbInSize, maxNtbInSize},
		{1 << 20, maxNtbInSize},
	} {
		if got := HostNtbInSize(c.advertised); got != c.want {
			t.Errorf("HostNtbInSize(%d) = %d, want %d", c.advertised, got, c.want)
		}
	}
}
