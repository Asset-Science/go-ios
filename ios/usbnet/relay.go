package usbnet

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/danielpaulus/go-ios/ios/golog"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// DialFunc opens a host-side connection; tests replace it, production uses net.Dialer.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

const (
	mtu            = 1500
	nicID          = 1
	dialTimeout    = 15 * time.Second
	udpIdleTimeout = 60 * time.Second
	outQueueLen    = 1024
	maxPendingTCP  = 1024
)

// Relay is the userspace TCP/IP stack between one phone and the host: every TCP connection and
// UDP flow the phone opens is terminated here and re-opened as an ordinary host socket, so the
// phone reaches whatever the host reaches. DNS the phone sends to the gateway goes to dnsUpstream.
type Relay struct {
	stack *stack.Stack
	ep    *channel.Endpoint
	lease Lease
	dns   netip.AddrPort
	dial  DialFunc

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

// NewRelay builds the stack for one link. dnsUpstream may be invalid, in which case DNS sent to
// the gateway is refused like any other connection to it.
func NewRelay(l Lease, dnsUpstream netip.AddrPort, dial DialFunc) (*Relay, error) {
	if dial == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		dial = d.DialContext
	}
	r := &Relay{
		stack: stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		}),
		ep:    channel.New(outQueueLen, mtu, ""),
		lease: l,
		dns:   dnsUpstream,
		dial:  dial,
		conns: map[net.Conn]struct{}{},
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	if err := r.stack.CreateNIC(nicID, r.ep); err != nil {
		return nil, errors.New("usbnet: CreateNIC: " + err.String())
	}
	host := tcpip.AddrFrom4(l.HostAddr.As4())
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: host, PrefixLen: 24}}
	if err := r.stack.AddProtocolAddress(nicID, addr, stack.AddressProperties{}); err != nil {
		return nil, errors.New("usbnet: AddProtocolAddress: " + err.String())
	}
	// Accept the phone's packets for any destination, and answer from any source address: the
	// stack stands in for every server the phone talks to.
	if err := r.stack.SetPromiscuousMode(nicID, true); err != nil {
		return nil, errors.New("usbnet: SetPromiscuousMode: " + err.String())
	}
	if err := r.stack.SetSpoofing(nicID, true); err != nil {
		return nil, errors.New("usbnet: SetSpoofing: " + err.String())
	}
	r.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	r.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(r.stack, 0, maxPendingTCP, r.handleTCP).HandlePacket)
	r.stack.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(r.stack, r.handleUDP).HandlePacket)
	return r, nil
}

// InjectIPv4 hands the stack one IPv4 packet the phone sent.
func (r *Relay) InjectIPv4(p []byte) {
	if len(p) < header.IPv4MinimumSize || header.IPVersion(p) != header.IPv4Version {
		return
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), p...))})
	r.ep.InjectInbound(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()
}

// ReadIPv4 blocks until the stack has an IPv4 packet for the phone; nil once ctx or the relay ends.
func (r *Relay) ReadIPv4(ctx context.Context) []byte {
	pkt := r.ep.ReadContext(ctx)
	if pkt == nil {
		return nil
	}
	defer pkt.DecRef()
	buf := pkt.ToBuffer()
	defer buf.Release()
	return buf.Flatten()
}

// Close tears down every relayed connection and the stack.
func (r *Relay) Close() {
	r.cancel()
	r.mu.Lock()
	for c := range r.conns {
		_ = c.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	r.ep.Close()
	r.stack.Close()
}

func (r *Relay) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return false
	}
	r.conns[c] = struct{}{}
	return true
}

func (r *Relay) untrack(c net.Conn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
	_ = c.Close()
}

// target decides where a phone flow to dst goes on the host side: the station's DNS for DNS sent to
// the gateway, dst itself for ordinary unicast, nowhere for loopback, link-local, multicast,
// broadcast or any other port on the gateway (the station's own services are not exposed).
func (r *Relay) target(dst netip.Addr, port uint16, network string) (string, bool) {
	if dst == r.lease.HostAddr {
		if port == 53 && r.dns.IsValid() {
			return r.dns.String(), true
		}
		return "", false
	}
	if !dst.Is4() || dst.IsLoopback() || dst.IsLinkLocalUnicast() || dst.IsMulticast() || dst.IsUnspecified() ||
		dst == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || (network == "udp" && (port == dhcpServer || port == dhcpClient)) {
		return "", false
	}
	return net.JoinHostPort(dst.String(), strconv.Itoa(int(port))), true
}

func addrOf(a tcpip.Address) netip.Addr {
	b := a.As4()
	return netip.AddrFrom4(b)
}

func (r *Relay) handleTCP(req *tcp.ForwarderRequest) {
	id := req.ID()
	dst, ok := r.target(addrOf(id.LocalAddress), id.LocalPort, "tcp")
	if !ok {
		req.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(r.ctx, dialTimeout)
	host, err := r.dial(ctx, "tcp", dst)
	cancel()
	if err != nil {
		golog.Debug("usbnet tcp dial failed", "module", logModule, "target", dst, "error", err)
		req.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := req.CreateEndpoint(&wq)
	if terr != nil {
		_ = host.Close()
		req.Complete(true)
		return
	}
	req.Complete(false)
	phone := gonet.NewTCPConn(&wq, ep)
	if !r.track(phone) || !r.track(host) {
		r.untrack(phone)
		r.untrack(host)
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		pipeTCP(phone, host)
		r.untrack(phone)
		r.untrack(host)
	}()
}

// pipeTCP copies both ways, passing a half-close through so request/response protocols finish.
func pipeTCP(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyHalf(a, b)
	go copyHalf(b, a)
	wg.Wait()
}

func (r *Relay) handleUDP(req *udp.ForwarderRequest) {
	id := req.ID()
	dst, ok := r.target(addrOf(id.LocalAddress), id.LocalPort, "udp")
	if !ok {
		return
	}
	var wq waiter.Queue
	ep, terr := req.CreateEndpoint(&wq)
	if terr != nil {
		return
	}
	phone := gonet.NewUDPConn(&wq, ep)
	if !r.track(phone) {
		_ = phone.Close()
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer r.untrack(phone)
		ctx, cancel := context.WithTimeout(r.ctx, dialTimeout)
		host, err := r.dial(ctx, "udp", dst)
		cancel()
		if err != nil {
			golog.Debug("usbnet udp dial failed", "module", logModule, "target", dst, "error", err)
			return
		}
		if !r.track(host) {
			_ = host.Close()
			return
		}
		defer r.untrack(host)
		relayUDP(phone, host)
	}()
}

// relayUDP moves datagrams both ways until either side has been idle for udpIdleTimeout.
func relayUDP(phone, host net.Conn) {
	done := make(chan struct{}, 2)
	copyDatagrams := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 65535)
		for {
			_ = src.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := src.Read(buf)
			if err != nil {
				return
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}
	go copyDatagrams(host, phone)
	go copyDatagrams(phone, host)
	<-done
	_ = phone.Close()
	_ = host.Close()
	<-done
}
