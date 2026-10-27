package usbnet

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/danielpaulus/go-ios/ios/golog"
)

// Transport is the phone's NCM data path: whole NCM transfer blocks in and out.
type Transport interface {
	// ReadNtb reads one NTB into buf. It returns (0, nil) when nothing arrived before the
	// transport's own short timeout, so callers can check their context between reads.
	ReadNtb(buf []byte) (int, error)
	// WriteNtb sends one NTB to the phone.
	WriteNtb(b []byte) error
	// Params are the phone's GET_NTB_PARAMETERS limits.
	Params() NtbParameters
}

// Stats is a snapshot of a running link.
type Stats struct {
	Leased      bool   `json:"leased"`       // the phone holds the DHCP lease (acked at least once)
	PhoneMAC    string `json:"phone_mac"`    // empty until the phone's first frame
	FramesIn    uint64 `json:"frames_in"`    // Ethernet frames from the phone
	FramesOut   uint64 `json:"frames_out"`   // Ethernet frames to the phone
	BadBlocks   uint64 `json:"bad_blocks"`   // transfers that were not a valid NTB
	ClientAddr  string `json:"client_addr"`  // the address the phone is given
	GatewayAddr string `json:"gateway_addr"` // the host side of the link
}

// Link runs one phone's network link until its context ends or the transport fails.
type Link struct {
	t     Transport
	lease Lease
	relay *Relay

	writeMu sync.Mutex
	seq     uint16

	phoneMAC  atomic.Pointer[[6]byte]
	leased    atomic.Bool
	framesIn  atomic.Uint64
	framesOut atomic.Uint64
	badBlocks atomic.Uint64
}

// NewLink builds the link and its relay; Run drives it.
func NewLink(t Transport, l Lease, dnsUpstream netip.AddrPort, dial DialFunc) (*Link, error) {
	relay, err := NewRelay(l, dnsUpstream, dial)
	if err != nil {
		return nil, err
	}
	return &Link{t: t, lease: l, relay: relay}, nil
}

// Stats returns a snapshot of the link's counters.
func (k *Link) Stats() Stats {
	s := Stats{
		Leased:      k.leased.Load(),
		FramesIn:    k.framesIn.Load(),
		FramesOut:   k.framesOut.Load(),
		BadBlocks:   k.badBlocks.Load(),
		ClientAddr:  k.lease.ClientAddr.String(),
		GatewayAddr: k.lease.HostAddr.String(),
	}
	if m := k.phoneMAC.Load(); m != nil {
		s.PhoneMAC = netMAC(*m)
	}
	return s
}

func netMAC(m [6]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 17)
	for i, b := range m {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hex[b>>4], hex[b&0x0F])
	}
	return string(out)
}

// Run reads the phone's frames and answers them until ctx ends or the transport fails; it always
// closes the relay before returning.
func (k *Link) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // stack -> phone
		defer wg.Done()
		for {
			p := k.relay.ReadIPv4(ctx)
			if p == nil {
				return
			}
			mac := k.phoneMAC.Load()
			if mac == nil {
				continue // nothing can be addressed to the phone before it has sent a frame
			}
			if err := k.send(ethernet(*mac, k.lease.HostMAC, etherTypeIPv4, p)); err != nil {
				golog.Debug("usbnet write to phone failed", "module", logModule, "error", err)
			}
		}
	}()
	err := k.readLoop(ctx)
	cancel()
	k.relay.Close()
	wg.Wait()
	return err
}

func (k *Link) readLoop(ctx context.Context) error {
	size := int(k.t.Params().NtbInMaxSize)
	if size <= 0 || size > 1<<16 {
		size = 1 << 15
	}
	buf := make([]byte, size)
	for ctx.Err() == nil {
		n, err := k.t.ReadNtb(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		frames, perr := ParseNtb(buf[:n])
		if perr != nil {
			k.badBlocks.Add(1)
			continue
		}
		for _, f := range frames {
			k.handleFrame(f)
		}
	}
	return nil
}

func (k *Link) handleFrame(f []byte) {
	if len(f) < ethernetLen {
		return
	}
	k.framesIn.Add(1)
	if k.phoneMAC.Load() == nil {
		var m [6]byte
		copy(m[:], f[6:12])
		k.phoneMAC.Store(&m)
	}
	if reply := arpReplyFor(f, k.lease); reply != nil {
		k.sendLogged(reply)
		return
	}
	if m, ok := parseDHCPRequest(f); ok {
		if t := dhcpReplyType(m, k.lease); t != 0 {
			if t == dhcpAck && !k.leased.Swap(true) {
				golog.Info("usbnet phone leased", "module", logModule, "client", k.lease.ClientAddr.String())
			}
			k.sendLogged(buildDHCPReply(m, k.lease, t))
		}
		return
	}
	if p := ipv4Payload(f); p != nil {
		k.relay.InjectIPv4(p)
	}
}

func (k *Link) sendLogged(frame []byte) {
	if err := k.send(frame); err != nil {
		golog.Debug("usbnet write to phone failed", "module", logModule, "error", err)
	}
}

var errFrameTooLarge = errors.New("usbnet: frame does not fit one NTB")

// send wraps one frame in a 16-bit NTB — the only format the phone accepts from the host.
func (k *Link) send(frame []byte) error {
	k.writeMu.Lock()
	defer k.writeMu.Unlock()
	block := BuildNtb16(k.seq, [][]byte{frame}, k.t.Params())
	if block == nil {
		return errFrameTooLarge
	}
	k.seq++
	if err := k.t.WriteNtb(block); err != nil {
		return err
	}
	k.framesOut.Add(1)
	return nil
}
