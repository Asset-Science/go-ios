package usbnet

import (
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type closingTransport struct {
	*fakeTransport
	closed atomic.Bool
}

func (c *closingTransport) Close() { c.closed.Store(true) }

func TestManagerRunsOneLinkPerPhone(t *testing.T) {
	var opened atomic.Int32
	var last *closingTransport
	m := newManager(func(udid string) (DeviceTransport, error) {
		opened.Add(1)
		last = &closingTransport{fakeTransport: &fakeTransport{in: make(chan []byte)}}
		return last, nil
	}, func() netip.AddrPort { return netip.AddrPort{} }, nil)

	require.NoError(t, m.Start("00008130-001608993C13803A"))
	require.NoError(t, m.Start("00008130001608993c13803a"), "same phone, either UDID form: no second link")
	assert.Equal(t, int32(1), opened.Load())

	st, ok := m.Status("00008130-001608993C13803A")
	require.True(t, ok)
	assert.True(t, st.Running)
	assert.Equal(t, "192.168.77.2", st.ClientAddr)

	assert.True(t, m.Stop("00008130-001608993C13803A"))
	assert.True(t, last.closed.Load(), "the transport is closed when the link stops")
	_, ok = m.Status("00008130-001608993C13803A")
	assert.False(t, ok)
	assert.False(t, m.Stop("00008130-001608993C13803A"), "stopping twice")
}

func TestManagerReportsAndRestartsAnEndedLink(t *testing.T) {
	gone := errors.New("device gone")
	var opened atomic.Int32
	m := newManager(func(string) (DeviceTransport, error) {
		if opened.Add(1) == 1 {
			return &closingTransport{fakeTransport: &fakeTransport{in: make(chan []byte), fail: gone}}, nil
		}
		return &closingTransport{fakeTransport: &fakeTransport{in: make(chan []byte)}}, nil
	}, func() netip.AddrPort { return netip.AddrPort{} }, nil)

	require.NoError(t, m.Start("A"))
	waitFor(t, func() bool { st, _ := m.Status("A"); return !st.Running })
	st, _ := m.Status("A")
	assert.Equal(t, "device gone", st.Error)

	require.NoError(t, m.Start("A"), "an ended link is started afresh")
	assert.Equal(t, int32(2), opened.Load())
	st, _ = m.Status("A")
	assert.True(t, st.Running)
	m.StopAll()
	_, ok := m.Status("A")
	assert.False(t, ok)
}

func TestManagerSurfacesOpenFailures(t *testing.T) {
	m := newManager(func(string) (DeviceTransport, error) { return nil, errors.New("not in network mode") },
		func() netip.AddrPort { return netip.AddrPort{} }, nil)
	assert.EqualError(t, m.Start("A"), "not in network mode")
	_, ok := m.Status("A")
	assert.False(t, ok)
}
