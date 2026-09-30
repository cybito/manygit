//go:build darwin || linux

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// Match the public guard's local GUI gate; SSH and headless sessions never
// contact the local input-source owner, even when they have a pseudo-terminal.
func localGUITerminal() bool {
	for _, key := range []string{"SSH_CLIENT", "SSH_CONNECTION", "SSH_TTY"} {
		if os.Getenv(key) != "" {
			return false
		}
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return false
	}
	_ = tty.Close()
	if runtime.GOOS == "darwin" {
		console, err := os.Stat("/dev/console")
		if err != nil {
			return false
		}
		stat, ok := console.Sys().(*syscall.Stat_t)
		return ok && stat.Uid == uint32(os.Getuid())
	}
	runtimeDir := fmt.Sprintf("/run/user/%d", os.Getuid())
	return os.Getenv("XDG_RUNTIME_DIR") == runtimeDir &&
		os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "unix:path="+runtimeDir+"/bus" &&
		(os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("DISPLAY") != "")
}
