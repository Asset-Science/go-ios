package usbnet

import (
	"context"
	"net/netip"
	"strings"
	"sync"

	"github.com/danielpaulus/go-ios/ios/golog"
)

// DeviceTransport is a Transport that can be closed: what OpenTransport returns.
type DeviceTransport interface {
	Transport
	Close()
}

// ShareStatus is what the server reports for one phone.
type ShareStatus struct {
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"` // why the last link ended, if it did
	Stats
}

// Manager runs at most one link per phone, keyed by UDID.
type Manager struct {
	open func(udid string) (DeviceTransport, error)
	dns  func() netip.AddrPort
	dial DialFunc

	mu    sync.Mutex
	links map[string]*share
}

type share struct {
	cancel context.CancelFunc
	link   *Link
	done   chan struct{}
	err    error // set before done closes
}

// NewManager returns a manager that opens phones with the platform transport and relays DNS to
// the station's resolver.
func NewManager() *Manager {
	return newManager(func(udid string) (DeviceTransport, error) { return OpenTransport(udid) }, SystemDNSServer, nil)
}

func newManager(open func(string) (DeviceTransport, error), dns func() netip.AddrPort, dial DialFunc) *Manager {
	return &Manager{open: open, dns: dns, dial: dial, links: map[string]*share{}}
}

func key(udid string) string { return strings.ToUpper(strings.ReplaceAll(udid, "-", "")) }

// Start opens the phone's NCM function and starts sharing. Starting a phone that is already
// shared is a no-op; a phone whose previous link ended is started afresh.
func (m *Manager) Start(udid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.links[key(udid)]; ok {
		select {
		case <-s.done:
		default:
			return nil
		}
	}
	t, err := m.open(udid)
	if err != nil {
		return err
	}
	link, err := NewLink(t, DefaultLease(), m.dns(), m.dial)
	if err != nil {
		t.Close()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &share{cancel: cancel, link: link, done: make(chan struct{})}
	m.links[key(udid)] = s
	go func() {
		s.err = link.Run(ctx)
		t.Close()
		if s.err != nil {
			golog.Info("usbnet link ended", "module", logModule, "udid", udid, "error", s.err)
		}
		close(s.done)
	}()
	golog.Info("usbnet sharing started", "module", logModule, "udid", udid)
	return nil
}

// Stop ends sharing for the phone and waits for its link to close; false if it was not shared.
func (m *Manager) Stop(udid string) bool {
	m.mu.Lock()
	s, ok := m.links[key(udid)]
	delete(m.links, key(udid))
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.cancel()
	<-s.done
	return true
}

// StopAll ends every link, for server shutdown.
func (m *Manager) StopAll() {
	m.mu.Lock()
	udids := make([]string, 0, len(m.links))
	for k := range m.links {
		udids = append(udids, k)
	}
	m.mu.Unlock()
	for _, u := range udids {
		m.Stop(u)
	}
}

// Status reports the phone's link; ok is false if it was never started (or was stopped).
func (m *Manager) Status(udid string) (ShareStatus, bool) {
	m.mu.Lock()
	s, ok := m.links[key(udid)]
	m.mu.Unlock()
	if !ok {
		return ShareStatus{}, false
	}
	st := ShareStatus{Running: true, Stats: s.link.Stats()}
	select {
	case <-s.done:
		st.Running = false
		if s.err != nil {
			st.Error = s.err.Error()
		}
	default:
	}
	return st, true
}
