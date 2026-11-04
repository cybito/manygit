//go:build !darwin && !linux

package main

// hc terminals reach the reporter, which disables itself on this platform.
func imeTerminal() bool { return hcTerminal() }
