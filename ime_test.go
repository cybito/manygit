package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rabeeh-ta/manygit/internal/config"
	"github.com/rabeeh-ta/manygit/internal/tui"
)

type imeObservedInput struct{ reads atomic.Int32 }

func (r *imeObservedInput) Read(_ []byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}

func TestIMEInitializationFailureDoesNotStartTUI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enable the Unix-socket IME reporter")
	}
	// HOME includes the full socket suffix: use a short root for Darwin's
	// Unix socket pathname limit rather than its long default temporary path.
	home, err := os.MkdirTemp("/tmp", "mg-ime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	path := filepath.Join(home, ".local/state/infra-as-code/ime-control/run/control.sock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := bufio.NewReader(conn).ReadBytes('\n'); err != nil {
			return
		}
		_, _ = io.WriteString(conn, "{\"ok\":false,\"error\":\"BACKEND_UNAVAILABLE\"}\n")
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})
	input := &imeObservedInput{}
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	err = runTUI(tui.New(config.Default(), home, nil, nil), true,
		tea.WithInput(input), tea.WithOutput(&output), tea.WithContext(ctx), tea.WithoutSignalHandler())
	if err == nil || !strings.Contains(err.Error(), "BACKEND_UNAVAILABLE") {
		t.Fatalf("initial protection error was not returned: %v", err)
	}
	if input.reads.Load() != 0 {
		t.Fatal("keyboard loop started before the initial ACK")
	}
	if output.Len() != 0 {
		t.Fatal("terminal was taken over before the initial ACK")
	}
}
