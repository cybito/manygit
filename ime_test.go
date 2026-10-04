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
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rabeeh-ta/manygit/internal/config"
	"github.com/rabeeh-ta/manygit/internal/tui"
)

type imeObservedInput struct{ reads atomic.Int32 }

func (r *imeObservedInput) Read(p []byte) (int, error) {
	if r.reads.Add(1) == 1 {
		p[0] = 'q'
		return 1, nil
	}
	return 0, io.EOF
}

func TestIMEInitializationFailureStillStartsTUI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enable the Unix-socket IME reporter")
	}
	t.Setenv("HERDR_IME_INTENT", "")
	if err := os.Unsetenv("HERDR_IME_INTENT"); err != nil {
		t.Fatal(err)
	}
	// HOME includes the full socket suffix: use a short root for Darwin's
	// Unix socket pathname limit rather than its long default temporary path.
	root, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp(root, "mg-ime-")
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
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
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
		_, _ = io.WriteString(conn, "{\"ok\":false,\"generation\":1,\"error\":\"BACKEND_UNAVAILABLE\"}\n")
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
	if err != nil {
		t.Fatalf("optional IME initialization leaked an error: %v", err)
	}
	if ctx.Err() != nil || input.reads.Load() == 0 || output.Len() == 0 {
		t.Fatal("ordinary TUI input/quit did not proceed after rejected initialization")
	}
}

func TestHerdrMarkerPrecedesSSHAndDisablesWithoutBlockingTUI(t *testing.T) {
	t.Setenv("SSH_TTY", "/dev/pts/fixture")
	t.Setenv("HERDR_IME_INTENT", "")
	if err := os.Unsetenv("HERDR_IME_INTENT"); err != nil {
		t.Fatal(err)
	}
	if imeTerminal() {
		t.Fatal("ordinary SSH session selected a local daemon lease")
	}
	for _, marker := range []string{"", "unsupported", "invalid", "1"} {
		t.Run(marker, func(t *testing.T) {
			t.Setenv("HERDR_IME_INTENT", marker)
			t.Setenv("HERDR_ENV", "")
			if !imeTerminal() {
				t.Fatal("marked SSH session silently took the ordinary bypass")
			}
			input := &imeObservedInput{}
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			err := runTUI(tui.New(config.Default(), "", nil, nil), false,
				tea.WithInput(input), tea.WithOutput(&output), tea.WithContext(ctx), tea.WithoutSignalHandler())
			if err != nil {
				t.Fatalf("invalid marker leaked a consumer error: %v", err)
			}
			if ctx.Err() != nil || input.reads.Load() == 0 || output.Len() == 0 {
				t.Fatal("invalid Herdr marker blocked ordinary interactive input/quit")
			}
		})
	}
}
