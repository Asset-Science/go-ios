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
	dnsIdleTimeout = 5 * time.Second // one answer per query; random source ports make one flow each
	outQueueLen    = 1024
	maxPendingTCP  = 1024 // gVisor's limit on handshakes in progress, not on open connections
	maxTCPFlows    = 256  // open relayed TCP connections per phone
	maxUDPFlows    = 128  // open relayed UDP flows per phone
	udpBufferSize  = 65535
)

// deniedPrefixes are destinations the phone is never relayed to: everything that is not the public
// internet. A phone on a diagnostics station is usually a customer's trade-in, so it gets what Wi-Fi
// on an isolated network would give it — the internet — and not the station's LAN, its CGNAT side,
// or reserved space. The station's resolver is reached only through the gateway's DNS (target).
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), // includes the link's own 192.168.77.0/24
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), // includes 255.255.255.255
}

// Relay is the userspace TCP/IP stack between one phone and the host: every TCP connection and
// UDP flow the phone opens to the public internet is terminated here and re-opened as an ordinary
// host socket. DNS the phone sends to the gateway goes to dnsUpstream. Nothing else on or around
// the station is reachable (target).
type Relay struct {
	stack *stack.Stack
	ep    *channel.Endpoint
	lease Lease
	dns   netip.AddrPort
	dial  DialFunc
	// hostAddrs are the station's own addresses, refused like the denied prefixes so a station
	// with a public address does not expose its services either.
	hostAddrs map[netip.Addr]struct{}

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	tcpFlows int
	udpFlows int
	wg       sync.WaitGroup
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
		ep:        channel.New(outQueueLen, mtu, ""),
		lease:     l,
		dns:       dnsUpstream,
		dial:      dial,
		hostAddrs: localAddrs(),
		conns:     map[net.Conn]struct{}{},
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	if err := r.stack.CreateNIC(nicID, r.ep); err != nil {
		return nil, errors.New("usbnet: CreateNIC: " + err.String())
	}
	host := tcpip.AddrFrom4(l.HostAddr.As4())
	addr := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: host, PrefixLen: l.prefixLen()}}
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

// localAddrs is the station's own IPv4 addresses (empty if they cannot be listed: the denied
// prefixes still cover every private and loopback one).
func localAddrs() map[netip.Addr]struct{} {
	out := map[netip.Addr]struct{}{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() {
				out[ip.Unmap()] = struct{}{}
			}
		}
	}
	return out
}

// beginFlow registers a new relayed flow and its first connection, within the per-phone limit.
// The WaitGroup is added under the same lock Close takes, so Close never misses a flow.
func (r *Relay) beginFlow(network string, c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return false
	}
	switch {
	case network == "tcp" && r.tcpFlows >= maxTCPFlows, network == "udp" && r.udpFlows >= maxUDPFlows:
		golog.Debug("usbnet flow limit reached", "module", logModule, "network", network)
		return false
	case network == "tcp":
		r.tcpFlows++
	default:
		r.udpFlows++
	}
	r.conns[c] = struct{}{}
	r.wg.Add(1)
	return true
}

// tcpFull reports the TCP limit before a host socket is opened; beginFlow still has the last word.
func (r *Relay) tcpFull() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tcpFlows >= maxTCPFlows
}

// endFlow is beginFlow's pair, run by the flow's goroutine as it exits.
func (r *Relay) endFlow(network string, conns ...net.Conn) {
	r.mu.Lock()
	for _, c := range conns {
		delete(r.conns, c)
	}
	if network == "tcp" {
		r.tcpFlows--
	} else {
		r.udpFlows--
	}
	r.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	r.wg.Done()
}

// track adds a flow's second connection so Close can interrupt it.
func (r *Relay) track(c net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return false
	}
	r.conns[c] = struct{}{}
	return true
}

// target decides where a phone flow to dst goes on the host side: the station's resolver for DNS
// sent to the gateway, dst itself for public unicast, and nowhere for anything else — the
// gateway's other ports, the denied prefixes, and the station's own addresses.
func (r *Relay) target(dst netip.Addr, port uint16, network string) (string, bool) {
	if dst == r.lease.HostAddr {
		if port == 53 && r.dns.IsValid() {
			return r.dns.String(), true
		}
		return "", false
	}
	if !dst.Is4() || (network == "udp" && (port == dhcpServer || port == dhcpClient)) {
		return "", false
	}
	for _, p := range deniedPrefixes {
		if p.Contains(dst) {
			return "", false
		}
	}
	if _, own := r.hostAddrs[dst]; own {
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
	if !ok || r.tcpFull() {
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
	if !r.beginFlow("tcp", phone) {
		_ = phone.Close()
		_ = host.Close()
		return
	}
	if !r.track(host) {
		_ = host.Close()
		r.endFlow("tcp", phone)
		return
	}
	go func() {
		defer r.endFlow("tcp", phone, host)
		pipeTCP(phone, host)
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
	if !r.beginFlow("udp", phone) {
		_ = phone.Close()
		return
	}
	idle := udpIdleTimeout
	if id.LocalPort == 53 {
		idle = dnsIdleTimeout
	}
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, dialTimeout)
		host, err := r.dial(ctx, "udp", dst)
		cancel()
		if err != nil {
			golog.Debug("usbnet udp dial failed", "module", logModule, "target", dst, "error", err)
			r.endFlow("udp", phone)
			return
		}
		if !r.track(host) {
			_ = host.Close()
			r.endFlow("udp", phone)
			return
		}
		defer r.endFlow("udp", phone, host)
		relayUDP(phone, host, idle)
	}()
}

// relayUDP moves datagrams both ways until either side has been idle for `idle`.
func relayUDP(phone, host net.Conn, idle time.Duration) {
	done := make(chan struct{}, 2)
	copyDatagrams := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, udpBufferSize)
		for {
			_ = src.SetReadDeadline(time.Now().Add(idle))
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
