package usbnet

import (
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTransport feeds the link NTBs and records what it writes back.
type fakeTransport struct {
	in      chan []byte
	mu      sync.Mutex
	written [][]byte
	fail    error
}

func (f *fakeTransport) ReadNtb(buf []byte) (int, error) {
	if f.fail != nil {
		return 0, f.fail
	}
	select {
	case b := <-f.in:
		return copy(buf, b), nil
	case <-time.After(20 * time.Millisecond):
		return 0, nil
	}
}

func (f *fakeTransport) WriteNtb(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, append([]byte(nil), b...))
	return nil
}

func (f *fakeTransport) Params() NtbParameters { return phoneParams }

func (f *fakeTransport) writes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.written...)
}

// ntb32 wraps one frame the way the phone does: NTH32, NDP32 at 16, the datagram after it.
func ntb32(frame []byte) []byte {
	const ndpLen = 32
	le := binary.LittleEndian
	block := make([]byte, nth32Length+ndpLen, nth32Length+ndpLen+len(frame))
	le.PutUint32(block, nth32Signature)
	le.PutUint16(block[4:], nth32Length)
	le.PutUint32(block[12:], nth32Length)
	le.PutUint32(block[16:], ndp32NoCrc)
	le.PutUint16(block[20:], ndpLen)
	le.PutUint32(block[32:], nth32Length+ndpLen)
	le.PutUint32(block[36:], uint32(len(frame)))
	block = append(block, frame...)
	le.PutUint32(block[8:], uint32(len(block)))
	return block
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLinkLeasesThePhoneOverNtb32InNtb16Out(t *testing.T) {
	ft := &fakeTransport{in: make(chan []byte, 8)}
	link, err := NewLink(ft, DefaultLease(), netip.AddrPort{}, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- link.Run(ctx) }()

	ft.in <- ntb32(clientDHCP([]byte{53, 1, dhcpDiscover}, [4]byte{}))
	waitFor(t, func() bool { return len(ft.writes()) == 1 })
	offer := ft.writes()[0]
	assert.Equal(t, uint32(nth16Signature), binary.LittleEndian.Uint32(offer), "host to phone is always NTB16")
	frames, err := ParseNtb(offer)
	require.NoError(t, err)
	require.Len(t, frames, 1)
	assert.Equal(t, []byte{dhcpOffer}, replyOption(frames[0], 53))
	assert.False(t, link.Stats().Leased)

	ft.in <- ntb32(clientDHCP([]byte{53, 1, dhcpRequest, 50, 4, 192, 168, 77, 2}, [4]byte{}))
	waitFor(t, func() bool { return link.Stats().Leased })

	ft.in <- ntb32(arpRequest([4]byte{192, 168, 77, 2}, [4]byte{192, 168, 77, 1}))
	waitFor(t, func() bool { return len(ft.writes()) == 3 })
	ft.in <- []byte("not an NTB at all")
	waitFor(t, func() bool { return link.Stats().BadBlocks == 1 })

	s := link.Stats()
	assert.Equal(t, "ae:df:a1:e2:4e:ff", s.PhoneMAC)
	assert.Equal(t, uint64(3), s.FramesIn)
	assert.Equal(t, uint64(3), s.FramesOut)
	assert.Equal(t, "192.168.77.2", s.ClientAddr)

	cancel()
	assert.NoError(t, <-done)
}

func TestLinkStopsWhenTheTransportFails(t *testing.T) {
	gone := errors.New("device gone")
	link, err := NewLink(&fakeTransport{in: make(chan []byte), fail: gone}, DefaultLease(), netip.AddrPort{}, nil)
	require.NoError(t, err)
	assert.ErrorIs(t, link.Run(context.Background()), gone)
}
