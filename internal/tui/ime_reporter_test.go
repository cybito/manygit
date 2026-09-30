package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rabeeh-ta/manygit/internal/config"
)

// serveTestIME uses real socket framing. The handler can reject an operation or
// leave it unacknowledged; successful handlers model foreground lease ownership.
func serveTestIME(t *testing.T, handle func(uint64, imeRequest) (imeResponse, bool)) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enable the Unix-socket IME reporter")
	}
	// Keep the pathname below Darwin's Unix socket limit, including on hosts
	// whose default temporary directory already has a long pathname.
	dir, err := os.MkdirTemp("/tmp", "manygit-ime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var connections []net.Conn
	var workers sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		var id uint64
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			id++
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			workers.Add(1)
			go func(conn net.Conn, id uint64) {
				defer workers.Done()
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var request imeRequest
					if err := json.Unmarshal(line, &request); err != nil {
						t.Errorf("decode IME request: %v", err)
						return
					}
					response, send := handle(id, request)
					if !send {
						continue
					}
					if err := json.NewEncoder(conn).Encode(response); err != nil {
						return
					}
				}
			}(conn, id)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-acceptDone
		mu.Lock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return path
}

// The real business update starts a shell command; only Init is shortened to
// avoid unrelated repository probing in this protection-failure regression.
type imeCommandModel struct{ Model }

func (m imeCommandModel) Init() tea.Cmd {
	return func() tea.Msg { return tea.KeyMsg{Type: tea.KeyEnter} }
}

func TestIMEFailedACKQuitsWithoutRunningCommand(t *testing.T) {
	for _, failure := range []string{"rejected", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			path := serveTestIME(t, func(_ uint64, _ imeRequest) (imeResponse, bool) {
				return imeResponse{Error: "BACKEND_UNAVAILABLE"}, failure != "timeout"
			})
			dir := t.TempDir()
			marker := filepath.Join(dir, "unprotected-command-ran")
			m := New(config.Default(), dir, nil, nil)
			m.shellPrompting = true
			m.shellDir = dir
			m.shellLoc = "fixture"
			m.shellCmd = "printf ran > unprotected-command-ran"
			m.imeReporter = newIMEReporter(path)
			m.imeFocused = true
			t.Cleanup(func() { _ = m.CloseIME() })
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			businessDispatched := false
			filter := func(model tea.Model, msg tea.Msg) tea.Msg {
				// Observe the real engine accepting the shell/probe batch. A marker
				// alone could miss a leaked child that starts just after Run returns.
				if _, ok := msg.(tea.BatchMsg); ok {
					businessDispatched = true
				}
				return IMEFilter(model, msg)
			}
			final, err := tea.NewProgram(imeCommandModel{m}, tea.WithContext(ctx),
				tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(),
				tea.WithoutSignalHandler(), tea.WithFilter(filter)).Run()
			if err != nil {
				t.Fatalf("protection failure did not quit cleanly: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("TUI kept running after a failed ACK")
			}
			stopped := final.(Model)
			failureErr := stopped.IMEFailure()
			if failureErr == nil {
				t.Fatal("final Model lost the IME failure")
			}
			if failure == "rejected" && !strings.Contains(failureErr.Error(), "BACKEND_UNAVAILABLE") {
				t.Fatalf("lost service rejection: %v", failureErr)
			}
			if failure == "timeout" {
				var netErr net.Error
				if !errors.As(failureErr, &netErr) || !netErr.Timeout() {
					t.Fatalf("expected ACK timeout, got %v", failureErr)
				}
			}
			if businessDispatched {
				t.Fatal("failed protection dispatched the shell command to Bubble Tea")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unprotected business command executed: stat=%v", err)
			}
		})
	}
}

func TestIMEBlurredBackgroundAndResumeKeepOtherOwner(t *testing.T) {
	var mu sync.Mutex
	var owner uint64
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch request.Op {
		case "activate", "resume":
			owner = id
		case "blur", "suspend", "close":
			if owner == id {
				owner = 0
			}
		}
		return imeResponse{OK: true, Generation: 1, Session: fmt.Sprint(id)}, true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	if err := m.imeReporter.report(m.imeState()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	model, _ := m.Update(tea.BlurMsg{})
	m = model.(Model)
	other := newIMEReporter(path)
	if err := other.report("text"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.close() })
	mu.Lock()
	otherOwner := owner
	mu.Unlock()
	if otherOwner == 0 {
		t.Fatal("fixture did not establish another foreground owner")
	}
	assertOtherOwner := func() {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if owner != otherOwner {
			t.Fatalf("blurred TUI reclaimed owner: got %d, want %d", owner, otherOwner)
		}
	}
	model, _ = m.Update(fetchDoneMsg{})
	m = model.(Model)
	assertOtherOwner()
	if _, ok := IMEFilter(m, tea.SuspendMsg{}).(tea.SuspendMsg); !ok {
		t.Fatal("inactive suspend unexpectedly failed")
	}
	model, _ = m.Update(tea.ResumeMsg{})
	m = model.(Model)
	assertOtherOwner()
	model, _ = m.Update(statusMsg{})
	m = model.(Model)
	assertOtherOwner()
	// A real focus must reacquire the lease even when refetch takes its cooldown
	// early return. Before that event, resume and background work stay inactive.
	m.lastFetch = time.Now()
	model, cmd := m.Update(tea.FocusMsg{})
	m = model.(Model)
	if cmd != nil {
		t.Fatal("cooldown unexpectedly scheduled a fetch")
	}
	mu.Lock()
	defer mu.Unlock()
	if owner == 0 || owner == otherOwner {
		t.Fatal("real focus failed to reacquire foreground ownership")
	}
}

func TestIMESuspendRejectionSurvivesFilterModelCopy(t *testing.T) {
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		if request.Op == "suspend" {
			return imeResponse{Error: "UNKNOWN"}, true
		}
		return imeResponse{OK: true, Generation: 1, Session: fmt.Sprint(id)}, true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	if err := m.imeReporter.report(m.imeState()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	if _, ok := IMEFilter(m, tea.SuspendMsg{}).(tea.QuitMsg); !ok {
		t.Fatal("rejected suspend reached Bubble Tea's internal suspension")
	}
	if err := m.IMEFailure(); err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
		t.Fatalf("original Model lost filter failure: %v", err)
	}
}
