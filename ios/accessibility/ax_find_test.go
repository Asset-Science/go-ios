package accessibility

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const knownEid = "89049032005008882600049966079268"

var eidPattern = regexp.MustCompile(`89[0-9]{30}`)

// fakeScreen replays a fixed list of labels as the inspector would: one per GetElement,
// wrapping round to the first after the last. errAt makes element n (0-based, counted across
// the session) fail; panicAt makes it panic.
type fakeScreen struct {
	labels  []string
	next    int
	calls   int
	errAt   int
	panicAt int
	closed  bool
}

func newFakeScreen(labels ...string) *fakeScreen {
	return &fakeScreen{labels: labels, errAt: -1, panicAt: -1}
}

func (f *fakeScreen) GetElement(context.Context) (AXElementData, error) {
	n := f.calls
	f.calls++
	if n == f.panicAt {
		panic("cannot unarchive")
	}
	if n == f.errAt || len(f.labels) == 0 {
		return AXElementData{}, errors.New("timeout")
	}
	label := f.labels[f.next%len(f.labels)]
	f.next++
	return AXElementData{SpokenDescription: label}, nil
}

func (f *fakeScreen) Close() error {
	f.closed = true
	return nil
}

func TestFindInPass_matchesWithSpacesRemoved(t *testing.T) {
	screen := newFakeScreen("Name, iPhone", "EID, 8904 9032 0050 0888 2600 0499 6607 9268, Button", "Model")
	value, label, walked, err := findInPass(context.Background(), screen, eidPattern, 80, time.Second)
	assert.NoError(t, err)
	assert.Equal(t, knownEid, value)
	assert.Equal(t, "EID, 8904 9032 0050 0888 2600 0499 6607 9268, Button", label)
	assert.Equal(t, 2, walked)
}

func TestFindInPass_stopsWhenTheInspectorWraps(t *testing.T) {
	screen := newFakeScreen("Settings", "General", "About")
	value, _, walked, err := findInPass(context.Background(), screen, eidPattern, 80, time.Second)
	assert.NoError(t, err)
	assert.Empty(t, value)
	assert.Equal(t, 3, walked, "the walk ends when the first element comes round again")
}

func TestFindInPass_stopsAtMaxElements(t *testing.T) {
	labels := make([]string, 200)
	for i := range labels {
		labels[i] = "row " + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	_, _, walked, err := findInPass(context.Background(), newFakeScreen(labels...), eidPattern, 80, time.Second)
	assert.NoError(t, err)
	assert.Equal(t, 80, walked)
}

func TestFindInPass_firstElementFailingIsAnError(t *testing.T) {
	screen := newFakeScreen("About")
	screen.errAt = 0
	_, _, walked, err := findInPass(context.Background(), screen, eidPattern, 80, time.Second)
	assert.Error(t, err)
	assert.Equal(t, 0, walked)
}

func TestFindInPass_laterElementFailingEndsTheWalkWithoutError(t *testing.T) {
	screen := newFakeScreen("Settings", "General", "About")
	screen.errAt = 2
	value, _, walked, err := findInPass(context.Background(), screen, eidPattern, 80, time.Second)
	assert.NoError(t, err)
	assert.Empty(t, value)
	assert.Equal(t, 2, walked)
}

func TestFindInPass_panicBecomesAnError(t *testing.T) {
	screen := newFakeScreen("Settings")
	screen.panicAt = 0
	_, _, _, err := findInPass(context.Background(), screen, eidPattern, 80, time.Second)
	assert.ErrorContains(t, err, "panicked")
}

// fakeClock advances only when the search sleeps, so the tests run instantly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time                           { return c.t }
func (c *fakeClock) sleep(_ context.Context, d time.Duration) { c.t = c.t.Add(d) }

func TestFindWithConnector_findsTheValueOnceTheOperatorReachesTheScreen(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	// The operator is on the home screen for two walks, then opens About.
	walks := 0
	screens := []*fakeScreen{
		newFakeScreen("Settings", "Camera"),
		newFakeScreen("Settings", "General"),
		newFakeScreen("Name, iPhone", "EID, "+knownEid+", Button"),
	}
	session := &switchingSession{screens: screens, walks: &walks}
	connects := 0
	connect := func(context.Context) (elementSession, error) {
		connects++
		return session, nil
	}
	result, err := findWithConnector(context.Background(), connect, eidPattern,
		FindOptions{Wait: time.Minute, Interval: 2 * time.Second}, clock.now, clock.sleep)
	assert.NoError(t, err)
	assert.True(t, result.Found)
	assert.Equal(t, knownEid, result.Value)
	assert.Equal(t, 3, result.Passes)
	assert.Equal(t, 1, connects, "one connection is kept across walks")
	assert.True(t, session.closed, "the connection is closed at the end")
}

// switchingSession shows screens[i] on walk i: a walk ends when its screen wraps.
type switchingSession struct {
	screens []*fakeScreen
	walks   *int
	closed  bool
	inWalk  int
}

func (s *switchingSession) GetElement(ctx context.Context) (AXElementData, error) {
	i := *s.walks
	if i >= len(s.screens) {
		i = len(s.screens) - 1
	}
	screen := s.screens[i]
	el, err := screen.GetElement(ctx)
	s.inWalk++
	if s.inWalk > len(screen.labels) {
		// This element is the wrap-around; the next walk sees the next screen.
		s.inWalk = 0
		*s.walks++
	}
	return el, err
}

func (s *switchingSession) Close() error {
	s.closed = true
	return nil
}

func TestFindWithConnector_notFoundWhenTimeRunsOut(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	connect := func(context.Context) (elementSession, error) {
		return newFakeScreen("Settings", "General"), nil
	}
	result, err := findWithConnector(context.Background(), connect, eidPattern,
		FindOptions{Wait: 10 * time.Second, Interval: 2 * time.Second}, clock.now, clock.sleep)
	assert.NoError(t, err)
	assert.False(t, result.Found)
	assert.Equal(t, 5, result.Passes)
	assert.LessOrEqual(t, clock.t.Sub(time.Unix(0, 0)), 10*time.Second)
}

func TestFindWithConnector_reconnectsAfterAFailedWalk(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	broken := newFakeScreen("Settings")
	broken.errAt = 0
	sessions := []*fakeScreen{broken, newFakeScreen("EID, " + knownEid + ", Button")}
	connects := 0
	connect := func(context.Context) (elementSession, error) {
		s := sessions[connects]
		connects++
		return s, nil
	}
	result, err := findWithConnector(context.Background(), connect, eidPattern,
		FindOptions{Wait: time.Minute}, clock.now, clock.sleep)
	assert.NoError(t, err)
	assert.True(t, result.Found)
	assert.Equal(t, 2, connects)
	assert.True(t, broken.closed, "the failed connection is closed before reconnecting")
}

func TestFindWithConnector_givesUpAfterRepeatedConnectFailures(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	connects := 0
	connect := func(context.Context) (elementSession, error) {
		connects++
		return nil, errors.New("Failed connecting to service, error code:3")
	}
	_, err := findWithConnector(context.Background(), connect, eidPattern,
		FindOptions{Wait: time.Hour, MaxConnectFailures: 3}, clock.now, clock.sleep)
	assert.ErrorContains(t, err, "error code:3")
	assert.Equal(t, 3, connects)
}

func TestFindWithConnector_neverConnectedBeforeTimeRunsOutIsAnError(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	connect := func(context.Context) (elementSession, error) {
		return nil, errors.New("Failed connecting to service, error code:3")
	}
	_, err := findWithConnector(context.Background(), connect, eidPattern,
		FindOptions{Wait: 3 * time.Second, Interval: 2 * time.Second, MaxConnectFailures: 10}, clock.now, clock.sleep)
	assert.ErrorContains(t, err, "could not connect")
}

func TestFindWithConnector_stopsWhenCancelled(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	connects := 0
	connect := func(context.Context) (elementSession, error) {
		connects++
		return newFakeScreen("Settings"), nil
	}
	result, err := findWithConnector(ctx, connect, eidPattern, FindOptions{Wait: time.Hour}, clock.now, clock.sleep)
	assert.NoError(t, err)
	assert.False(t, result.Found)
	assert.Equal(t, 0, connects)
}
