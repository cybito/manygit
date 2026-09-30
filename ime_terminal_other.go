//go:build !darwin && !linux

package main

// No local IME reporter is enabled on Windows or other unsupported platforms.
func localGUITerminal() bool { return false }
