//go:build !darwin && !linux

package main

import "os"

// Marked sessions must fail visibly rather than silently bypassing transport.
func imeTerminal() bool {
	_, marked := os.LookupEnv("HERDR_IME_INTENT")
	return marked
}
