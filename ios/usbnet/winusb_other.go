//go:build !windows

package usbnet

import "errors"

// WinUSBTransport exists only on Windows; macOS shares its network with the phone natively.
type WinUSBTransport struct{}

// OpenTransport is Windows-only.
func OpenTransport(string) (*WinUSBTransport, error) {
	return nil, errors.New("usbnet: USB network sharing is only implemented on Windows")
}

// ReadNtb implements Transport.
func (*WinUSBTransport) ReadNtb([]byte) (int, error) { return 0, errors.ErrUnsupported }

// WriteNtb implements Transport.
func (*WinUSBTransport) WriteNtb([]byte) error { return errors.ErrUnsupported }

// Params implements Transport.
func (*WinUSBTransport) Params() NtbParameters { return NtbParameters{} }

// Close implements the Windows transport's Close.
func (*WinUSBTransport) Close() {}
