//go:build darwin || linux

package tui

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// The immediate socket directory is the private boundary. Above it, only
// current-UID/root-owned, non-writable ancestry is trusted; a root-owned sticky
// temporary directory is permitted, but symlinks are never followed.
func validateIMESocket(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("IME socket must have a clean absolute local path")
	}
	uid := uint32(os.Getuid())
	parent := filepath.Dir(path)
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect IME socket path: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("IME socket path has untrusted identity or symlink ancestry")
		}
		switch current {
		case path:
			if info.Mode()&os.ModeSocket == 0 || stat.Uid != uid || info.Mode().Perm()&0o077 != 0 {
				return errors.New("IME socket must be private and owned by the current UID")
			}
		case parent:
			if !info.IsDir() || stat.Uid != uid || info.Mode().Perm()&0o077 != 0 {
				return errors.New("IME socket directory must be private and owned by the current UID")
			}
		default:
			stickyRoot := stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
			if !info.IsDir() || (stat.Uid != 0 && stat.Uid != uid) ||
				(info.Mode().Perm()&0o022 != 0 && !stickyRoot) {
				return errors.New("IME socket has unsafe directory ancestry")
			}
		}
		if current == string(filepath.Separator) {
			return nil
		}
	}
}

// Herdr coalesces unchanged intent, but a real input episode must still reject
// an already closed stream. Peek never consumes an ACK or waits for idle data.
func probeIMESocket(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("IME connection is not a local Unix stream")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var n int
	var receiveErr error
	var byteBuf [1]byte
	if err := raw.Control(func(fd uintptr) {
		n, _, receiveErr = syscall.Recvfrom(int(fd), byteBuf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
	}); err != nil {
		return err
	}
	if errors.Is(receiveErr, syscall.EAGAIN) || errors.Is(receiveErr, syscall.EWOULDBLOCK) {
		return nil
	}
	if receiveErr != nil {
		return fmt.Errorf("probe IME stream: %w", receiveErr)
	}
	if n == 0 {
		return io.EOF
	}
	return errors.New("unsolicited Herdr intent stream data")
}
