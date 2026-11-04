//go:build !darwin && !linux

package tui

import (
	"errors"
	"net"
)

func validateIMESocket(string) error {
	return errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func validateHCSocket(string) (imeSocketIdentity, error) {
	return imeSocketIdentity{}, errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func imeProcessGroup() (int, error) {
	return 0, errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}

func probeIMESocket(net.Conn) error {
	return errors.New("IME_CONTROL_UNSUPPORTED_PLATFORM")
}
