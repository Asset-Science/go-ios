package accessibility

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/golog"
)

// FindOptions bounds FindOnScreen. Every field has a default, so the zero value is usable.
type FindOptions struct {
	// Wait is how long to keep looking. The operator may still be navigating to the screen
	// when the search starts, so the screen is walked again every Interval until Wait runs out.
	Wait time.Duration
	// Interval is the pause between two walks of the screen.
	Interval time.Duration
	// MaxElements caps one walk. The inspector wraps around to the first element at the end
	// of the screen; the cap ends a walk whose screen changed under it, so it never wraps.
	MaxElements int
	// PerElement caps the wait for one element.
	PerElement time.Duration
	// MaxConnectFailures ends the search after this many failed connections in a row.
	MaxConnectFailures int
}

// FindResult is what FindOnScreen reports. Value is the matched text with spaces removed;
// Label is the element's full spoken description. When nothing matched, Seen holds the labels
// of the last walk, so the caller can tell a page that was not open from a label the pattern
// does not cover.
type FindResult struct {
	Found     bool     `json:"found"`
	Value     string   `json:"value,omitempty"`
	Label     string   `json:"label,omitempty"`
	Elements  int      `json:"elements"`
	Passes    int      `json:"passes"`
	ElapsedMs int64    `json:"elapsedMs"`
	Seen      []string `json:"seen,omitempty"`
}

// maxSeenLabelLength caps each label in FindResult.Seen.
const maxSeenLabelLength = 80

func (o FindOptions) withDefaults() FindOptions {
	if o.Wait <= 0 {
		o.Wait = 30 * time.Second
	}
	if o.Interval <= 0 {
		o.Interval = 2 * time.Second
	}
	if o.MaxElements <= 0 {
		o.MaxElements = 80
	}
	if o.PerElement <= 0 {
		o.PerElement = 4 * time.Second
	}
	if o.MaxConnectFailures <= 0 {
		o.MaxConnectFailures = 3
	}
	return o
}

// elementSource is the part of ControlInterface a walk needs, so the walk is testable without a device.
type elementSource interface {
	GetElement(ctx context.Context) (AXElementData, error)
}

// elementSession is one inspector connection.
type elementSession interface {
	elementSource
	Close() error
}

// findInPass walks the current screen once and returns the first element whose spoken
// description, with spaces removed, matches re. The walk ends at the first match, at
// maxElements, when the first element comes round again (the inspector wrapped), or when an
// element does not arrive within perElement. A walk that returned no element at all is an
// error, so the caller reconnects; a walk that saw elements but no match is not. seen holds
// the labels the walk read, in order.
func findInPass(ctx context.Context, src elementSource, re *regexp.Regexp, maxElements int, perElement time.Duration) (value, label string, walked int, seen []string, err error) {
	defer func() {
		// awaitHostInspectorCurrentElementChanged panics on a payload it cannot unarchive.
		// One bad element must end this walk, not the process.
		if r := recover(); r != nil {
			err = fmt.Errorf("element read panicked: %v", r)
		}
	}()
	first := ""
	for walked < maxElements {
		elementCtx, cancel := context.WithTimeout(ctx, perElement)
		el, getErr := src.GetElement(elementCtx)
		cancel()
		if getErr != nil {
			if walked == 0 {
				return "", "", 0, seen, getErr
			}
			return "", "", walked, seen, nil
		}
		if walked > 0 && el.SpokenDescription == first {
			return "", "", walked, seen, nil
		}
		if walked == 0 {
			first = el.SpokenDescription
		}
		walked++
		seen = append(seen, truncateLabel(el.SpokenDescription))
		if m := re.FindString(strings.ReplaceAll(el.SpokenDescription, " ", "")); m != "" {
			return m, el.SpokenDescription, walked, seen, nil
		}
	}
	return "", "", walked, seen, nil
}

// findWithConnector is FindOnScreen with the connection, clock and sleep injected for tests.
// It keeps one connection across walks and opens a new one only after a walk fails. It returns
// an error only when it could not connect MaxConnectFailures times in a row; running out of
// time without a match is a result with Found false. A search that never connected at all
// also returns the last connection error, so the caller can tell "not on screen" from "could
// not look".
func findWithConnector(ctx context.Context, connect func(context.Context) (elementSession, error), re *regexp.Regexp, opts FindOptions,
	now func() time.Time, sleep func(context.Context, time.Duration)) (FindResult, error) {
	opts = opts.withDefaults()
	start := now()
	deadline := start.Add(opts.Wait)
	result := FindResult{}
	var session elementSession
	closeSession := func() {
		if session != nil {
			session.Close()
			session = nil
		}
	}
	defer closeSession()
	connectFailures := 0
	var connectErr error
	connected := false
	for {
		if ctx.Err() != nil {
			break
		}
		if session == nil {
			s, err := connect(ctx)
			if err != nil {
				connectFailures++
				connectErr = err
				if connectFailures >= opts.MaxConnectFailures {
					result.ElapsedMs = now().Sub(start).Milliseconds()
					return result, fmt.Errorf("could not connect to the accessibility inspector %d times in a row: %w", connectFailures, err)
				}
			} else {
				session = s
				connected = true
				connectFailures = 0
			}
		}
		if session != nil {
			value, label, walked, seen, err := findInPass(ctx, session, re, opts.MaxElements, opts.PerElement)
			result.Passes++
			result.Elements = walked
			if walked > 0 {
				result.Seen = seen
			}
			golog.Debug("ax find walk", "module", logModule, "pass", result.Passes, "elements", walked, "matched", value != "", "error", err)
			if value != "" {
				result.Found, result.Value, result.Label = true, value, label
				result.Seen = nil
				break
			}
			if err != nil {
				closeSession()
			}
		}
		if !now().Add(opts.Interval).Before(deadline) {
			break
		}
		sleep(ctx, opts.Interval)
	}
	result.ElapsedMs = now().Sub(start).Milliseconds()
	if !connected && connectErr != nil {
		return result, fmt.Errorf("could not connect to the accessibility inspector: %w", connectErr)
	}
	return result, nil
}

// FindOnScreen watches the device's screen through the accessibility inspector until an
// element's spoken description (spaces removed) matches re, or opts.Wait runs out. It reads
// text only: no screenshot and no developer disk image, but the inspector service needs
// Developer Mode on iOS 16+.
func FindOnScreen(ctx context.Context, device ios.DeviceEntry, re *regexp.Regexp, opts FindOptions) (FindResult, error) {
	connect := func(ctx context.Context) (elementSession, error) {
		conn, err := New(ctx, device, silentNotifier{})
		if err != nil {
			if conn != nil {
				conn.Close()
			}
			return nil, err
		}
		// SwitchToDevice waits for the device's answer with no deadline of its own.
		if !runWithin(opts.withDefaults().PerElement, func() {
			conn.SwitchToDevice()
			conn.EnableSelectionMode()
		}) {
			conn.Close()
			return nil, fmt.Errorf("the accessibility inspector did not answer within %s", opts.withDefaults().PerElement)
		}
		return &inspectorSession{conn: conn, limit: opts.withDefaults().PerElement}, nil
	}
	sleep := func(ctx context.Context, d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	return findWithConnector(ctx, connect, re, opts, time.Now, sleep)
}

// inspectorSession turns the inspector's on-screen selection off before closing, so the phone
// is left as it was found.
type inspectorSession struct {
	conn  *ControlInterface
	limit time.Duration
}

func (s *inspectorSession) GetElement(ctx context.Context) (AXElementData, error) {
	return s.conn.GetElement(ctx)
}

func (s *inspectorSession) Close() error {
	// TurnOff, like SwitchToDevice, waits for the device with no deadline of its own.
	runWithin(s.limit, s.conn.TurnOff)
	return s.conn.Close()
}

// runWithin runs fn and reports whether it returned within limit. A call that overruns is left
// running in its goroutine; closing the connection afterwards ends it.
func runWithin(limit time.Duration, fn func()) bool {
	done := make(chan struct{})
	go func() {
		defer func() {
			recover()
			close(done)
		}()
		fn()
	}()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// silentNotifier drops the inspector's notifications, which a one-shot search does not use.
type silentNotifier struct{}

func (silentNotifier) HostAppStateChanged(Notification)               {}
func (silentNotifier) HostInspectorNotificationReceived(Notification) {}

func truncateLabel(label string) string {
	r := []rune(label)
	if len(r) > maxSeenLabelLength {
		return string(r[:maxSeenLabelLength]) + "..."
	}
	return label
}
