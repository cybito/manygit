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
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rabeeh-ta/manygit/internal/config"
)

type imeResponse struct {
	OK         bool   `json:"ok"`
	Generation uint64 `json:"generation"`
	Session    string `json:"session,omitempty"`
	Scope      string `json:"scope,omitempty"`
	Error      string `json:"error,omitempty"`
}

// These fixtures exercise real framing and connection ownership; the direct
// wrapper and Herdr open/operation handlers share the same Unix stream.
func serveTestIME(t *testing.T, handle func(uint64, imeRequest) (imeResponse, bool)) string {
	t.Helper()
	return serveTestIMEFrames(t, func(id uint64, line []byte) ([]byte, bool) {
		var request imeRequest
		if err := json.Unmarshal(line, &request); err != nil {
			t.Errorf("decode IME request: %v", err)
			return nil, false
		}
		response, send := handle(id, request)
		frame, err := json.Marshal(response)
		if err != nil {
			t.Errorf("encode IME response: %v", err)
			return nil, false
		}
		return frame, send
	})
}

func serveTestIMEFrames(t *testing.T, handle func(uint64, []byte) ([]byte, bool), afterSend ...func(net.Conn)) string {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("platform does not enable the Unix-socket IME reporter")
	}
	root, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "manygit-ime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
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
					response, send := handle(id, line)
					if !send {
						continue
					}
					if response == nil {
						return // explicit EOF fixture, not an unacknowledged RPC
					}
					if _, err := conn.Write(append(response, '\n')); err != nil {
						return
					}
					for _, after := range afterSend {
						after(conn)
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

func appliedTestIMEACK(id uint64) imeResponse {
	return imeResponse{OK: true, Generation: 1, Session: fmt.Sprint(id), Scope: "applied"}
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
				return imeResponse{Generation: 1, Error: "BACKEND_UNAVAILABLE"}, failure != "timeout"
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
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
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
		return appliedTestIMEACK(id), true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	if err := m.imeReporter.episode(m.imeState()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	model, _ := m.Update(tea.BlurMsg{})
	m = model.(Model)
	other := newIMEReporter(path)
	if err := other.episode("text"); err != nil {
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
			return imeResponse{Generation: 1, Error: "UNKNOWN"}, true
		}
		return appliedTestIMEACK(id), true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	if err := m.imeReporter.episode(m.imeState()); err != nil {
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

func TestIMEInactiveWaitsForRealInputOrFocus(t *testing.T) {
	for _, regain := range []string{"key", "focus"} {
		t.Run(regain, func(t *testing.T) {
			var foreground atomic.Bool
			var requests atomic.Int32
			var owner atomic.Uint64
			path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
				requests.Add(1)
				response := appliedTestIMEACK(id)
				if request.Op == "activate" || request.Op == "resume" || request.Op == "state" {
					if !foreground.Load() {
						response.Scope = "inactive"
						owner.Store(0)
					} else {
						owner.Store(id)
					}
				}
				return response, true
			})
			m := New(config.Default(), "", nil, nil)
			m.imeReporter = newIMEReporter(path)
			if err := m.EnableTUIIME(); err != nil {
				t.Fatalf("inactive startup became fatal: %v", err)
			}
			t.Cleanup(func() { _ = m.CloseIME() })
			if m.imeFocused || m.imeReporter.authorized() {
				t.Fatal("inactive ACK retained command authorization")
			}
			for _, msg := range []tea.Msg{fetchDoneMsg{}, statusMsg{}, openDoneMsg{}, tea.ResumeMsg{}} {
				next, _ := m.Update(msg)
				m = next.(Model)
			}
			if requests.Load() != 1 || owner.Load() != 0 {
				t.Fatal("background completion reacquired an inactive lease")
			}
			next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			m = next.(Model)
			if m.filtering || cmd != nil || m.IMEFailure() != nil {
				t.Fatal("inactive real input dispatched a command or latched a failure")
			}
			foreground.Store(true)
			m.lastFetch = time.Now()
			if regain == "key" {
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			} else {
				// Sample current text mode at the new focus episode, not the
				// command mode that was refused before losing focus.
				m.filtering = true
				next, _ = m.Update(tea.FocusMsg{})
			}
			m = next.(Model)
			if !m.filtering || !m.imeFocused || owner.Load() == 0 || m.IMEFailure() != nil {
				t.Fatal("genuine foreground episode failed to restore the current text classifier")
			}
			if m.imeReporter.state != "text" {
				t.Fatalf("replayed stale command mode: %s", m.imeReporter.state)
			}
		})
	}
}

func TestIMEStateInactiveCancelsOldAuthorization(t *testing.T) {
	var requests atomic.Int32
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		requests.Add(1)
		response := appliedTestIMEACK(id)
		if request.Op == "state" {
			response.Scope = "inactive"
		}
		return response, true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if m.imeFocused || m.imeReporter.authorized() || m.IMEFailure() != nil {
		t.Fatal("state inactive did not cancel prior applied authorization normally")
	}
	before := requests.Load()
	next, _ = m.Update(fetchDoneMsg{})
	m = next.(Model)
	if requests.Load() != before {
		t.Fatal("business update retried acquisition after state became inactive")
	}
	m.lastFetch = time.Now()
	next, _ = m.Update(tea.FocusMsg{})
	m = next.(Model)
	if !m.imeFocused || m.imeReporter.state != "text" {
		t.Fatal("focus did not explicitly acquire the freshly sampled text mode")
	}
}

func TestIMEEditorHandoffDoesNotResumeOnCompletion(t *testing.T) {
	var owner atomic.Uint64
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		switch request.Op {
		case "activate", "resume":
			owner.Store(id)
		case "suspend", "blur", "close":
			owner.CompareAndSwap(id, 0)
		}
		return appliedTestIMEACK(id), true
	})
	cfg, repos := twoRepos(t)
	cfg.OpenCmd = "true"
	m := New(cfg, "", repos, nil)
	m.imeReporter = newIMEReporter(path)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	parentOwner := owner.Load()
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m = next.(Model)
	if cmd == nil || owner.Load() != 0 || m.imeReporter.authorized() {
		t.Fatal("editor launch was not preceded by synchronous parent suspension")
	}
	child := newIMEReporter(path)
	if err := child.episode("command"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.close() })
	childOwner := owner.Load()
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, _ = m.Update(statusMsg{})
	m = next.(Model)
	if owner.Load() != childOwner || m.imeReporter.authorized() {
		t.Fatal("editor completion/background work stole the child lease")
	}
	if err := child.close(); err != nil {
		t.Fatal(err)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if owner.Load() != parentOwner || !m.filtering || !m.imeFocused {
		t.Fatal("true input did not resume the preserved parent stream")
	}
}

func TestIMEACKIdentityGenerationAndScopeRejectionsLatch(t *testing.T) {
	for _, defect := range []string{"session", "generation", "missing-scope", "recorded", "pending", "unknown"} {
		t.Run(defect, func(t *testing.T) {
			var requests atomic.Int32
			path := serveTestIME(t, func(id uint64, _ imeRequest) (imeResponse, bool) {
				response := appliedTestIMEACK(id)
				response.Generation = 8
				if requests.Add(1) == 2 {
					switch defect {
					case "session":
						response.Session = "other"
					case "generation":
						response.Generation = 7
					case "missing-scope":
						response.Scope = ""
					case "recorded":
						response.Scope = "recorded"
					case "pending":
						response.Scope = "pending"
					case "unknown":
						response.Scope = "future"
					}
				}
				return response, true
			})
			reporter := newIMEReporter(path)
			if err := reporter.episode("command"); err != nil {
				t.Fatal(err)
			}
			first := reporter.report("text")
			if first == nil || reporter.authorized() {
				t.Fatal("bad ACK retained source authorization")
			}
			if again := reporter.episode("command"); again != first || requests.Load() != 2 {
				t.Fatal("fatal ACK failure reconnected/replayed or replaced its first error")
			}
			_ = reporter.close()
		})
	}
}

func TestIMEBlurPendingDoesNotAuthorizeBackgroundInput(t *testing.T) {
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		response := appliedTestIMEACK(id)
		if request.Op == "blur" {
			response.Scope = "pending"
		}
		return response, true
	})
	reporter := newIMEReporter(path)
	if err := reporter.episode("command"); err != nil {
		t.Fatal(err)
	}
	if err := reporter.blur(); err != nil || reporter.authorized() || reporter.error() != nil {
		t.Fatalf("pending blur retained authorization or became fatal: %v", err)
	}
	if err := reporter.close(); err != nil {
		t.Fatal(err)
	}
}

func TestIMEACKFrameBoundary(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
				frame := `{"ok":true,"generation":1,"session":"fixture","scope":"applied"}`
				return []byte(strings.Repeat(" ", size-len(frame)-1) + frame), true
			})
			reporter := newIMEReporter(path)
			err := reporter.episode("command")
			if (err == nil) != (size == 4096) || reporter.authorized() != (size == 4096) {
				t.Fatalf("frame limit %d: authorization=%v error=%v", size, reporter.authorized(), err)
			}
			// Closing the valid boundary case receives the same valid frame.
			_ = reporter.close()
		})
	}
}

func TestHerdrIMEPaneAndPopupRecordOnly(t *testing.T) {
	for _, target := range []string{"pane", "popup"} {
		t.Run(target, func(t *testing.T) {
			var requests atomic.Int32
			var opened atomic.Bool
			path := serveTestIMEFrames(t, func(_ uint64, line []byte) ([]byte, bool) {
				var request struct {
					ID     string            `json:"id"`
					Method string            `json:"method"`
					Params map[string]string `json:"params"`
					Op     string            `json:"op"`
					State  string            `json:"state"`
					Policy string            `json:"policy"`
				}
				if err := json.Unmarshal(line, &request); err != nil {
					t.Errorf("decode Herdr frame: %v", err)
					return nil, false
				}
				requests.Add(1)
				if request.Method != "" {
					key := "pane_id"
					if target == "popup" {
						key = "popup_terminal_id"
					}
					if request.ID != "ime:open" || request.Method != "pane.input_intent.stream" ||
						len(request.Params) != 1 || request.Params[key] != "target" || opened.Swap(true) {
						t.Errorf("incorrect or repeated Herdr stream open: %+v", request)
					}
					return []byte(`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"herdr-owner","generation":3}}`), true
				}
				if !opened.Load() {
					t.Error("lifecycle request before Herdr stream open")
				}
				return []byte(`{"ok":true,"generation":3,"session":"herdr-owner","scope":"recorded"}`), true
			})
			t.Setenv("HERDR_IME_INTENT", "1")
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_SOCKET_PATH", path)
			t.Setenv("HERDR_PANE_ID", "")
			t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "")
			t.Setenv("SSH_TTY", "/dev/pts/herdr-fixture")
			if target == "pane" {
				t.Setenv("HERDR_PANE_ID", "target")
			} else {
				t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "target")
			}
			m := New(config.Default(), "", nil, nil)
			if err := m.EnableTUIIME(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = m.CloseIME() })
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
			m = next.(Model)
			if !m.filtering || m.imeReporter.state != "text" || m.IMEFailure() != nil {
				t.Fatal("recorded text mode prevented enabled Herdr app input")
			}
			next, _ = m.Update(tea.BlurMsg{})
			m = next.(Model)
			before := requests.Load()
			next, _ = m.Update(fetchDoneMsg{})
			m = next.(Model)
			if requests.Load() != before {
				t.Fatal("background Herdr update reactivated a blurred session")
			}
			m.lastFetch = time.Now()
			next, _ = m.Update(tea.FocusMsg{})
			m = next.(Model)
			if !m.imeFocused || m.imeReporter.session != "herdr-owner" || !opened.Load() {
				t.Fatal("Herdr focus episode lost its intent-stream identity")
			}
		})
	}
}

func TestHerdrIMEInvalidLaunchNeverFallsBack(t *testing.T) {
	for _, defect := range []string{"empty-marker", "unsupported", "marker", "env", "no-target", "two-targets",
		"no-socket", "relative", "unclean", "public-socket", "public-parent", "symlink"} {
		t.Run(defect, func(t *testing.T) {
			var contacted atomic.Int32
			path := serveTestIME(t, func(id uint64, _ imeRequest) (imeResponse, bool) {
				contacted.Add(1)
				return appliedTestIMEACK(id), true
			})
			t.Setenv("HERDR_IME_INTENT", "1")
			t.Setenv("HERDR_ENV", "1")
			t.Setenv("HERDR_SOCKET_PATH", path)
			t.Setenv("HERDR_PANE_ID", "pane")
			t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "")
			switch defect {
			case "empty-marker":
				t.Setenv("HERDR_IME_INTENT", "")
			case "unsupported":
				t.Setenv("HERDR_IME_INTENT", "unsupported")
			case "marker":
				t.Setenv("HERDR_IME_INTENT", "yes")
			case "env":
				t.Setenv("HERDR_ENV", "")
			case "no-target":
				t.Setenv("HERDR_PANE_ID", "")
			case "two-targets":
				t.Setenv("HERDR_IME_POPUP_TERMINAL_ID", "popup")
			case "no-socket":
				t.Setenv("HERDR_SOCKET_PATH", "")
			case "relative":
				t.Setenv("HERDR_SOCKET_PATH", "control.sock")
			case "unclean":
				t.Setenv("HERDR_SOCKET_PATH", filepath.Dir(path)+"/../"+filepath.Base(filepath.Dir(path))+"/control.sock")
			case "public-socket":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "public-parent":
				if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := filepath.Join(filepath.Dir(path), "alias.sock")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HERDR_SOCKET_PATH", link)
			}
			m := New(config.Default(), "", nil, nil)
			if err := m.EnableTUIIME(); err == nil {
				t.Fatal("invalid Herdr launch silently selected a working transport")
			}
			if contacted.Load() != 0 || m.imeReporter != nil {
				t.Fatal("invalid Herdr launch contacted a service or retained a lease")
			}
		})
	}
}

func TestHerdrIMEOpenACKBoundaries(t *testing.T) {
	for _, frame := range []string{
		`{"id":"other","result":{"type":"pane_input_intent_stream_opened","session":"s","generation":1}}`,
		`{"id":"ime:open","result":{"type":"ok","session":"s","generation":1}}`,
		`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"","generation":1}}`,
		`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"s","generation":0}}`,
		`{"id":"ime:open","error":{"code":"TARGET_NOT_FOUND","message":"gone"}}`,
		`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"s","generation":1}} {}`,
		strings.Repeat(" ", 4097),
	} {
		t.Run(fmt.Sprintf("frame-%d", len(frame)), func(t *testing.T) {
			var requests atomic.Int32
			path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
				requests.Add(1)
				return []byte(frame), true
			})
			reporter := &imeReporter{path: path, target: map[string]string{"pane_id": "target"}}
			first := reporter.episode("command")
			if first == nil || reporter.authorized() {
				t.Fatal("invalid stream open allowed a lifecycle activation")
			}
			if again := reporter.episode("text"); again != first || requests.Load() != 1 {
				t.Fatal("invalid stream open reconnected or replayed a mode")
			}
			_ = reporter.close()
		})
	}
}

func TestIMEEditorSuspendFailureNeverLaunchesChild(t *testing.T) {
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		if request.Op == "suspend" {
			return imeResponse{Generation: 1, Error: "UNKNOWN"}, true
		}
		return appliedTestIMEACK(id), true
	})
	cfg, repos := twoRepos(t)
	script := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > \"$0.ran\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg.OpenCmd = script
	m := New(cfg, "", repos, nil)
	m.imeReporter = newIMEReporter(path)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	m = next.(Model)
	if cmd == nil {
		t.Fatal("failed handoff did not terminate the TUI")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Fatal("failed handoff returned an editor command instead of shutdown")
	}
	if m.IMEFailure() == nil || !strings.Contains(m.IMEFailure().Error(), "UNKNOWN") {
		t.Fatal("editor handoff lost its first protection failure")
	}
	if _, err := os.Stat(script + ".ran"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("editor launched after failed release: %v", err)
	}
}

func TestIMEACKMalformedEnvelopeNeverAuthorizes(t *testing.T) {
	for i, frame := range []string{
		`{"generation":1,"error":"UNKNOWN"}`,
		`{"ok":null,"generation":1,"error":"UNKNOWN"}`,
		`{"ok":true,"session":"s","scope":"applied"}`,
		`{"ok":true,"generation":1.5,"session":"s","scope":"applied"}`,
		`{"ok":true,"generation":-1,"session":"s","scope":"applied"}`,
		`{"ok":true,"generation":1,"session":"s","scope":"applied","extra":true}`,
		`{"ok":true,"generation":1,"session":"s","scope":"applied"} {}`,
		`{"ok":false,"generation":1,"error":"UNKNOWN","session":"s"}`,
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
				return []byte(frame), true
			})
			reporter := newIMEReporter(path)
			if err := reporter.episode("command"); err == nil || reporter.authorized() {
				t.Fatal("malformed ACK authorized command dispatch")
			}
			_ = reporter.close()
		})
	}
}

func TestIMETransportEOFDoesNotReconnectOrReplay(t *testing.T) {
	var requests atomic.Int32
	path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
		if requests.Add(1) == 1 {
			return []byte(`{"ok":true,"generation":1,"session":"s","scope":"applied"}`), true
		}
		return nil, true
	})
	reporter := newIMEReporter(path)
	if err := reporter.episode("command"); err != nil {
		t.Fatal(err)
	}
	first := reporter.report("text")
	if !errors.Is(first, io.EOF) || reporter.authorized() {
		t.Fatalf("EOF did not invalidate authorization: %v", first)
	}
	if again := reporter.episode("command"); again != first || requests.Load() != 2 {
		t.Fatal("EOF reconnected or replayed a previous classifier")
	}
	_ = reporter.close()
}

func TestHerdrIMEOpenTimeoutNeverSendsMode(t *testing.T) {
	var requests atomic.Int32
	path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
		requests.Add(1)
		return nil, false
	})
	reporter := &imeReporter{path: path, target: map[string]string{"pane_id": "target"}}
	first := reporter.episode("command")
	var timeout net.Error
	if !errors.As(first, &timeout) || !timeout.Timeout() || reporter.authorized() {
		t.Fatalf("open did not fail with a bounded transport timeout: %v", first)
	}
	if again := reporter.episode("text"); again != first || requests.Load() != 1 {
		t.Fatal("open timeout sent a mode/reconnected instead of retaining its failure")
	}
	_ = reporter.close()
}

func TestHerdrIMELifecycleMustMatchRecordedOpenIdentity(t *testing.T) {
	for _, defect := range []string{"scope", "session", "generation"} {
		t.Run(defect, func(t *testing.T) {
			var requests atomic.Int32
			path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
				if requests.Add(1) == 1 {
					return []byte(`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"s","generation":3}}`), true
				}
				response := imeResponse{OK: true, Generation: 3, Session: "s", Scope: "recorded"}
				switch defect {
				case "scope":
					response.Scope = "applied"
				case "session":
					response.Session = "other"
				case "generation":
					response.Generation = 2
				}
				frame, err := json.Marshal(response)
				if err != nil {
					t.Errorf("encode Herdr lifecycle ACK: %v", err)
					return nil, true
				}
				return frame, true
			})
			reporter := &imeReporter{path: path, target: map[string]string{"pane_id": "target"}}
			first := reporter.episode("command")
			if first == nil || reporter.authorized() {
				t.Fatal("Herdr accepted a source ACK or mismatched its recorded stream identity")
			}
			if again := reporter.episode("text"); again != first || requests.Load() != 2 {
				t.Fatal("invalid lifecycle ACK re-opened/replayed its mode")
			}
			_ = reporter.close()
		})
	}
}

func TestIMEJobControlResumeWaitsForTrueInput(t *testing.T) {
	var owner atomic.Uint64
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		switch request.Op {
		case "activate", "resume":
			owner.Store(id)
		case "blur", "suspend", "close":
			owner.CompareAndSwap(id, 0)
		}
		return appliedTestIMEACK(id), true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	parentOwner := owner.Load()
	next, suspend := m.Update(tea.KeyMsg{Type: tea.KeyCtrlZ})
	m = next.(Model)
	if suspend == nil {
		t.Fatal("Ctrl-Z did not request native suspension")
	}
	if _, ok := IMEFilter(m, suspend()).(tea.SuspendMsg); !ok {
		t.Fatal("valid Ctrl-Z suspension failed")
	}
	if owner.Load() != 0 {
		t.Fatal("Ctrl-Z terminal handoff retained the parent source owner")
	}
	other := newIMEReporter(path)
	if err := other.episode("text"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.close() })
	otherOwner := owner.Load()
	next, _ = m.Update(tea.ResumeMsg{})
	m = next.(Model)
	if owner.Load() != otherOwner || m.imeReporter.authorized() {
		t.Fatal("job-control callback reacquired without a genuine input/focus episode")
	}
	if err := other.close(); err != nil {
		t.Fatal(err)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if owner.Load() != parentOwner || !m.filtering || !m.imeFocused {
		t.Fatal("true input did not resume the preserved stream after job-control handoff")
	}
}

func TestIMEKeyRevalidatesCompositorOnlyPause(t *testing.T) {
	var foreground atomic.Bool
	foreground.Store(true)
	var paused atomic.Bool
	var requests atomic.Int32
	var owner atomic.Uint64
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		requests.Add(1)
		response := appliedTestIMEACK(id)
		if request.Op == "activate" || request.Op == "resume" {
			if !foreground.Load() {
				response.Scope = "inactive"
				owner.Store(0)
			} else {
				paused.Store(false)
				owner.Store(id)
			}
		}
		return response, true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	// The compositor paused the daemon owner without any terminal BlurMsg;
	// the reporter still holds its old active flag.
	foreground.Store(false)
	paused.Store(true)
	owner.Store(0)
	before := requests.Load()
	next, _ := m.Update(fetchDoneMsg{})
	m = next.(Model)
	if requests.Load() != before || owner.Load() != 0 || !paused.Load() {
		t.Fatal("background work reclaimed a compositor-paused daemon lease")
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if cmd != nil || m.filtering || m.imeFocused || m.IMEFailure() != nil || owner.Load() != 0 {
		t.Fatal("cached active flag authorized command input after a compositor-only pause")
	}
	foreground.Store(true)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if !m.filtering || !m.imeFocused || paused.Load() || owner.Load() == 0 {
		t.Fatal("real foreground key did not resume the latest classifier")
	}
}

func TestHerdrCoalescedInputRejectsStreamEOF(t *testing.T) {
	var requests atomic.Int32
	closed := make(chan struct{})
	path := serveTestIMEFrames(t, func(_ uint64, _ []byte) ([]byte, bool) {
		if requests.Add(1) == 1 {
			return []byte(`{"id":"ime:open","result":{"type":"pane_input_intent_stream_opened","session":"s","generation":3}}`), true
		}
		return []byte(`{"ok":true,"generation":3,"session":"s","scope":"recorded"}`), true
	}, func(conn net.Conn) {
		if requests.Load() == 2 {
			_ = conn.Close()
			close(closed)
		}
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = &imeReporter{path: path, target: map[string]string{"pane_id": "pane"}}
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	<-closed
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if m.filtering || m.imeReporter.authorized() || !errors.Is(m.IMEFailure(), io.EOF) {
		t.Fatal("coalesced Herdr intent hid a dead stream and dispatched input")
	}
	if cmd == nil {
		t.Fatal("dead Herdr stream did not terminate the TUI")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit || requests.Load() != 2 {
		t.Fatal("dead Herdr stream reconnected/replayed rather than retaining its failure")
	}
}
