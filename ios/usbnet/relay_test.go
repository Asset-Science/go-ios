package usbnet

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// fakeHost records what the relay dials and hands back one end of an in-memory pipe.
type fakeHost struct {
	mu     sync.Mutex
	dialed []string
	conns  chan net.Conn
	refuse bool
}

func (h *fakeHost) dial(_ context.Context, network, address string) (net.Conn, error) {
	h.mu.Lock()
	h.dialed = append(h.dialed, network+" "+address)
	h.mu.Unlock()
	if h.refuse {
		return nil, &net.OpError{Op: "dial", Net: network, Err: io.EOF}
	}
	relaySide, testSide := net.Pipe()
	h.conns <- testSide
	return relaySide, nil
}

func (h *fakeHost) dials() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialed...)
}

// phoneStack is a second userspace stack standing in for the iPhone (192.168.77.2), wired to the
// relay packet-for-packet the way the USB link wires the real phone.
func phoneStack(t *testing.T, r *Relay) *stack.Stack {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	ep := channel.New(256, mtu, "")
	require.Nil(t, s.CreateNIC(1, ep))
	require.Nil(t, s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{192, 168, 77, 2}), PrefixLen: 24}},
		stack.AddressProperties{}))
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // phone -> relay
		defer wg.Done()
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			buf := pkt.ToBuffer()
			r.InjectIPv4(buf.Flatten())
			buf.Release()
			pkt.DecRef()
		}
	}()
	go func() { // relay -> phone
		defer wg.Done()
		for {
			p := r.ReadIPv4(ctx)
			if p == nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(p)})
			ep.InjectInbound(ipv4.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		ep.Close()
		wg.Wait()
		s.Close()
	})
	return s
}

func newTestRelay(t *testing.T, h *fakeHost) *Relay {
	r, err := NewRelay(DefaultLease(), netip.MustParseAddrPort("10.0.1.127:53"), h.dial)
	require.NoError(t, err)
	t.Cleanup(r.Close)
	return r
}

func TestRelayCarriesThePhonesTCPToTheHostAndBack(t *testing.T) {
	h := &fakeHost{conns: make(chan net.Conn, 4)}
	r := newTestRelay(t, h)
	phone := phoneStack(t, r)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, phone, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{17, 253, 24, 195}), Port: 443}, ipv4.ProtocolNumber)
	require.NoError(t, err)
	defer conn.Close()

	var host net.Conn
	select {
	case host = <-h.conns:
	case <-ctx.Done():
		t.Fatal("the relay never dialed the host")
	}
	assert.Equal(t, []string{"tcp 17.253.24.195:443"}, h.dials())

	_, err = conn.Write([]byte("client hello"))
	require.NoError(t, err)
	got := make([]byte, 12)
	_, err = io.ReadFull(host, got)
	require.NoError(t, err)
	assert.Equal(t, "client hello", string(got))

	_, err = host.Write([]byte("server hello"))
	require.NoError(t, err)
	_, err = io.ReadFull(conn, got)
	require.NoError(t, err)
	assert.Equal(t, "server hello", string(got))

	require.NoError(t, conn.CloseWrite()) // half-close reaches the host as EOF
	_, err = host.Read(got)
	assert.ErrorIs(t, err, io.EOF)
	_ = host.Close()
}

func TestRelaySendsDNSForTheGatewayToTheStationsResolver(t *testing.T) {
	h := &fakeHost{conns: make(chan net.Conn, 4)}
	r := newTestRelay(t, h)
	phone := phoneStack(t, r)

	gateway := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 168, 77, 1}), Port: 53}
	conn, err := gonet.DialUDP(phone, nil, &gateway, ipv4.ProtocolNumber)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("query"))
	require.NoError(t, err)

	var host net.Conn
	select {
	case host = <-h.conns:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay never dialed the resolver")
	}
	assert.Equal(t, []string{"udp 10.0.1.127:53"}, h.dials())
	q := make([]byte, 5)
	_, err = io.ReadFull(host, q)
	require.NoError(t, err)
	assert.Equal(t, "query", string(q))

	_, err = host.Write([]byte("answer"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	a := make([]byte, 16)
	n, err := conn.Read(a)
	require.NoError(t, err)
	assert.Equal(t, "answer", string(a[:n]))
}

func TestRelayRefusesTheStationsOwnServices(t *testing.T) {
	h := &fakeHost{conns: make(chan net.Conn, 4)}
	r := newTestRelay(t, h)
	phone := phoneStack(t, r)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gonet.DialContextTCP(ctx, phone, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 168, 77, 1}), Port: 80}, ipv4.ProtocolNumber)
	assert.Error(t, err, "the gateway's other ports are not relayed")
	_, err = gonet.DialContextTCP(ctx, phone, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{127, 0, 0, 1}), Port: 17890}, ipv4.ProtocolNumber)
	assert.Error(t, err, "loopback is never relayed")
	assert.Empty(t, h.dials())
}

func TestRelayResetsWhenTheHostCannotConnect(t *testing.T) {
	h := &fakeHost{conns: make(chan net.Conn, 4), refuse: true}
	r := newTestRelay(t, h)
	phone := phoneStack(t, r)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gonet.DialContextTCP(ctx, phone, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{17, 253, 24, 195}), Port: 443}, ipv4.ProtocolNumber)
	assert.Error(t, err)
	assert.Equal(t, []string{"tcp 17.253.24.195:443"}, h.dials())
}

func TestRelayTargetPolicy(t *testing.T) {
	r := &Relay{lease: DefaultLease(), dns: netip.MustParseAddrPort("10.0.1.127:53")}
	for _, c := range []struct {
		dst     string
		port    uint16
		network string
		want    string
		ok      bool
	}{
		{"17.253.24.195", 443, "tcp", "17.253.24.195:443", true},
		{"192.168.77.1", 53, "udp", "10.0.1.127:53", true},
		{"192.168.77.1", 53, "tcp", "10.0.1.127:53", true},
		{"192.168.77.1", 80, "tcp", "", false},
		{"127.0.0.1", 80, "tcp", "", false},
		{"169.254.1.1", 80, "tcp", "", false},
		{"224.0.0.251", 5353, "udp", "", false},
		{"255.255.255.255", 67, "udp", "", false},
		{"8.8.8.8", 67, "udp", "", false},
		{"0.0.0.0", 1, "tcp", "", false},
	} {
		got, ok := r.target(netip.MustParseAddr(c.dst), c.port, c.network)
		assert.Equal(t, c.ok, ok, c.dst)
		assert.Equal(t, c.want, got, c.dst)
	}
	noDNS := &Relay{lease: DefaultLease()}
	_, ok := noDNS.target(netip.MustParseAddr("192.168.77.1"), 53, "udp")
	assert.False(t, ok, "no upstream resolver: DNS to the gateway is refused")
}
