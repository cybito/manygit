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
// wrapper and hc label handlers share the same Unix stream.
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

func TestIMEFailedACKContinuesRunningCommand(t *testing.T) {
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
				if out, ok := msg.(scriptOutMsg); ok && out.done {
					return tea.QuitMsg{}
				}
				return IMEFilter(model, msg)
			}
			final, err := tea.NewProgram(imeCommandModel{m}, tea.WithContext(ctx),
				tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(),
				tea.WithoutSignalHandler(), tea.WithFilter(filter)).Run()
			if err != nil {
				t.Fatalf("optional reporter interrupted the program: %v", err)
			}
			if ctx.Err() != nil {
				t.Fatal("shell command did not complete after a failed ACK")
			}
			stopped := final.(Model)
			failureErr := stopped.imeReporter.error()
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
			if !businessDispatched {
				t.Fatal("optional reporter swallowed the shell command")
			}
			if contents, err := os.ReadFile(marker); err != nil || string(contents) != "ran" {
				t.Fatalf("ordinary shell command did not execute: %q, %v", contents, err)
			}
			if stopped.imeReporter.authorized() || stopped.imeReporter.conn != nil || stopped.imeReporter.state != "" {
				t.Fatal("disabled reporter retained active intent")
			}
		})
	}
}

func TestIMEBlurredBackgroundAndResumeKeepOtherOwner(t *testing.T) {
	var mu sync.Mutex
	var owner uint64
	var parentForeground atomic.Bool
	parentForeground.Store(true)
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		mu.Lock()
		defer mu.Unlock()
		switch request.Op {
		case "activate", "resume":
			if id == 1 && !parentForeground.Load() {
				response := appliedTestIMEACK(id)
				response.Scope = "inactive"
				return response, true
			}
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
	parentForeground.Store(false)
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
	model, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
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
	parentForeground.Store(true)
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

func TestIMEExplicitBlurBlocksInputAndReturnWithoutDaemonFocusChecks(t *testing.T) {
	var calls atomic.Int64
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		calls.Add(1)
		return appliedTestIMEACK(id), true
	})
	r := newIMEReporter(path)
	t.Cleanup(func() { _ = r.close() })
	if err := r.episode("command"); err != nil {
		t.Fatal(err)
	}
	if err := r.blur(); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for _, state := range []string{"command", "text"} {
		if err := r.key(state); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != before || r.authorized() {
		t.Fatal("input reacquired an explicitly blurred lease")
	}
	if err := r.suspend(); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if err := r.returned("command"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before || r.authorized() {
		t.Fatal("child return overrode explicit blur")
	}
	if err := r.episode("command"); err != nil {
		t.Fatal(err)
	}
	if !r.authorized() {
		t.Fatal("real focus failed to restore authorization")
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
	if _, ok := IMEFilter(m, tea.SuspendMsg{}).(tea.SuspendMsg); !ok {
		t.Fatal("rejected suspend blocked native suspension")
	}
	if err := m.imeReporter.error(); err == nil || !strings.Contains(err.Error(), "UNKNOWN") {
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
			if m.filtering || cmd != nil || m.imeReporter.error() != nil {
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
			if !m.filtering || !m.imeFocused || owner.Load() == 0 || m.imeReporter.error() != nil {
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
	if m.imeFocused || m.imeReporter.authorized() || m.imeReporter.error() != nil {
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

func TestIMEEditorHandoffReclaimsBeforeLateChildClose(t *testing.T) {
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
	next, _ = m.Update(cmd())
	m = next.(Model)
	next, _ = m.Update(statusMsg{})
	m = next.(Model)
	if owner.Load() != parentOwner || !m.imeReporter.authorized() {
		t.Fatal("editor return did not reclaim its own command mode before input")
	}
	if err := child.close(); err != nil {
		t.Fatal(err)
	}
	if owner.Load() != parentOwner {
		t.Fatal("late child close released the returned parent")
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

type hcLabelRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		Terminal string            `json:"terminal"`
		Labels   map[string]string `json:"labels"`
	} `json:"params"`
}

// hcReply confirms the requested label exactly as herdr-core does.
func hcReply(t *testing.T, line []byte) (hcLabelRequest, []byte) {
	t.Helper()
	var request hcLabelRequest
	if err := json.Unmarshal(line, &request); err != nil {
		t.Errorf("decode hc frame: %v", err)
	}
	labels := map[string]string{"other": "kept"}
	if value := request.Params.Labels["imekit.intent"]; value != "" {
		labels["imekit.intent"] = value
	}
	frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID,
		"result": map[string]any{"terminal": request.Params.Terminal, "labels": labels}})
	if err != nil {
		t.Errorf("encode hc reply: %v", err)
	}
	return request, frame
}

func setHCEnv(t *testing.T, terminal, socket string) {
	t.Setenv("HC_TERMINAL_ID", terminal)
	t.Setenv("HC_TERMINAL_SOCKET", socket)
}

func TestHCLabelLifecycle(t *testing.T) {
	var mu sync.Mutex
	var values []string
	path := serveTestIMEFrames(t, func(_ uint64, line []byte) ([]byte, bool) {
		request, frame := hcReply(t, line)
		if request.JSONRPC != "2.0" || request.Method != "terminal.label" || request.Params.Terminal != "t7" ||
			len(request.Params.Labels) != 1 {
			t.Errorf("incorrect hc request: %s", line)
		}
		mu.Lock()
		values = append(values, request.Params.Labels["imekit.intent"])
		mu.Unlock()
		return frame, true
	})
	_ = os.Chmod(path, 0o755) // herdr-core leaves its socket 0755 in a 0700 directory
	setHCEnv(t, "t7", path)
	t.Setenv("SSH_TTY", "/dev/pts/hc-fixture")
	pgid, err := imeProcessGroup()
	if err != nil {
		t.Fatal(err)
	}
	label := func(state string) string { return fmt.Sprintf("1:%d:%s", pgid, state) }
	m := New(config.Default(), "", nil, nil)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	if !m.imeFocused || m.imeReporter.session != "hc:t7" || m.imeReporter.generation != 1 {
		t.Fatal("hc activate did not return a recorded synthetic ACK")
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if !m.filtering || m.imeReporter.state != "text" || m.imeReporter.error() != nil {
		t.Fatal("recorded text mode prevented input")
	}
	next, _ = m.Update(tea.BlurMsg{})
	m = next.(Model)
	r := m.imeReporter
	r.mu.Lock()
	scope, err := r.exchange(imeRequest{Op: "state", State: "command"})
	r.mu.Unlock()
	if err != nil || scope != "recorded" || r.generation != 4 {
		t.Fatalf("unheld state was not accepted: %q %v", scope, err)
	}
	m.lastFetch = time.Now()
	next, _ = m.Update(tea.FocusMsg{})
	m = next.(Model)
	IMEFilter(m, tea.SuspendMsg{})
	m.filtering = false
	next, _ = m.Update(tea.ResumeMsg{})
	m = next.(Model)
	if !m.imeFocused || m.imeReporter.state != "command" {
		t.Fatal("hc return did not declare its current mode")
	}
	_ = m.CloseIME()
	mu.Lock()
	defer mu.Unlock()
	want := []string{label("command"), label("text"), label("idle"), label("text"), label("idle"), label("command"), ""}
	if strings.Join(values, ",") != strings.Join(want, ",") {
		t.Fatalf("hc labels = %q, want %q", values, want)
	}
}

func TestHCEnterWritesCommand(t *testing.T) {
	var value atomic.Value
	path := serveTestIMEFrames(t, func(_ uint64, line []byte) ([]byte, bool) {
		request, frame := hcReply(t, line)
		value.Store(request.Params.Labels["imekit.intent"])
		return frame, true
	})
	r := &imeReporter{path: path, terminal: "t1"}
	if scope, err := r.exchange(imeRequest{Op: "enter"}); err != nil || scope != "recorded" || !r.held ||
		!strings.HasSuffix(value.Load().(string), ":command") {
		t.Fatalf("enter did not hold command: %v", err)
	}
	_ = r.close()
}

func TestHCInvalidLaunchNeverFallsBack(t *testing.T) {
	for _, defect := range []string{"only-id", "only-socket", "relative", "unclean", "public-parent", "symlink", "missing"} {
		t.Run(defect, func(t *testing.T) {
			var contacted atomic.Int32
			path := serveTestIME(t, func(id uint64, _ imeRequest) (imeResponse, bool) {
				contacted.Add(1)
				return appliedTestIMEACK(id), true
			})
			t.Setenv("SSH_TTY", "")
			setHCEnv(t, "t1", path)
			switch defect {
			case "only-id":
				t.Setenv("HC_TERMINAL_SOCKET", "")
			case "only-socket":
				t.Setenv("HC_TERMINAL_ID", "")
			case "relative":
				t.Setenv("HC_TERMINAL_SOCKET", "control.sock")
			case "unclean":
				t.Setenv("HC_TERMINAL_SOCKET", filepath.Dir(path)+"/../"+filepath.Base(filepath.Dir(path))+"/control.sock")
			case "public-parent":
				if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				link := filepath.Join(filepath.Dir(path), "alias.sock")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				t.Setenv("HC_TERMINAL_SOCKET", link)
			case "missing":
				t.Setenv("HC_TERMINAL_SOCKET", filepath.Join(filepath.Dir(path), "missing.sock"))
			}
			m := New(config.Default(), "", nil, nil)
			if err := m.EnableTUIIME(); err != nil {
				t.Fatalf("invalid hc launch leaked a consumer error: %v", err)
			}
			if contacted.Load() != 0 || m.imeReporter.error() == nil || m.imeReporter.authorized() {
				t.Fatal("invalid hc launch contacted a service or retained a lease")
			}
		})
	}
}

func TestHCReplyValidationDisables(t *testing.T) {
	for name, reply := range map[string]func(hcLabelRequest) string{
		"error": func(r hcLabelRequest) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"x"}}`, r.ID)
		},
		"id": func(r hcLabelRequest) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"labels":{}}}`, r.ID+1)
		},
		"no-labels": func(r hcLabelRequest) string { return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, r.ID) },
		"value": func(r hcLabelRequest) string {
			return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"labels":{"imekit.intent":"1:1:idle"}}}`, r.ID)
		},
		"oversize": func(hcLabelRequest) string { return strings.Repeat(" ", 4097) },
		"timeout":  func(hcLabelRequest) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			var requests atomic.Int32
			path := serveTestIMEFrames(t, func(_ uint64, line []byte) ([]byte, bool) {
				requests.Add(1)
				var request hcLabelRequest
				_ = json.Unmarshal(line, &request)
				frame := reply(request)
				return []byte(frame), frame != ""
			})
			r := &imeReporter{path: path, terminal: "t1"}
			first := r.episode("command")
			if first == nil || r.authorized() {
				t.Fatal("invalid hc reply authorized intent")
			}
			if again := r.episode("text"); again != first || requests.Load() != 1 {
				t.Fatal("hc failure reconnected or replayed")
			}
			_ = r.close()
		})
	}
}

func TestIMEHandoffReturnDeclaresCurrentModeBeforeInput(t *testing.T) {
	for _, completion := range []tea.Msg{tea.ResumeMsg{}, openDoneMsg{}} {
		for _, editing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%T/text=%v", completion, editing), func(t *testing.T) {
				var declaration atomic.Value
				path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
					declaration.Store(request)
					return appliedTestIMEACK(id), true
				})
				m := New(config.Default(), "", nil, nil)
				m.imeReporter = newIMEReporter(path)
				_ = m.EnableTUIIME()
				t.Cleanup(func() { _ = m.CloseIME() })
				if err := m.imeReporter.suspend(); err != nil {
					t.Fatal(err)
				}
				m.filtering = editing
				next, _ := m.Update(completion)
				m = next.(Model)
				state := "command"
				if editing {
					state = "text"
				}
				got := declaration.Load().(imeRequest)
				if !m.imeFocused || got.Op != "activate" || got.State != state {
					t.Fatalf("terminal returned without current mode before input: focused=%v request=%+v", m.imeFocused, got)
				}
			})
		}
	}
}

func TestIMEJobControlReturnReclaimsBeforeInput(t *testing.T) {
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
	next, _ = m.Update(tea.ResumeMsg{})
	m = next.(Model)
	if owner.Load() != parentOwner || !m.imeReporter.authorized() {
		t.Fatal("terminal return did not declare its current command mode before input")
	}
	if err := other.close(); err != nil {
		t.Fatal(err)
	}
	if owner.Load() != parentOwner {
		t.Fatal("late child close released the returned parent")
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
	if cmd != nil || m.filtering || m.imeFocused || m.imeReporter.error() != nil || owner.Load() != 0 {
		t.Fatal("cached active flag authorized command input after a compositor-only pause")
	}
	foreground.Store(true)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if !m.filtering || !m.imeFocused || paused.Load() || owner.Load() == 0 {
		t.Fatal("real foreground key did not resume the latest classifier")
	}
}

func TestHCCoalescedInputRejectsStreamEOF(t *testing.T) {
	var requests atomic.Int32
	closed := make(chan struct{})
	path := serveTestIMEFrames(t, func(_ uint64, line []byte) ([]byte, bool) {
		requests.Add(1)
		_, frame := hcReply(t, line)
		return frame, true
	}, func(conn net.Conn) {
		if requests.Load() == 1 {
			_ = conn.Close()
			close(closed)
		}
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = &imeReporter{path: path, terminal: "t1"}
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.CloseIME() })
	<-closed
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(Model)
	if !m.filtering || m.imeReporter.authorized() || !errors.Is(m.imeReporter.error(), io.EOF) {
		t.Fatal("dead hc connection failed to disable enhancement and preserve input")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m = next.(Model)
	if m.filter != "x" || requests.Load() != 1 || m.imeReporter.conn != nil {
		t.Fatal("disabled reporter swallowed text or reconnected/replayed")
	}
}

func TestIMEMissingSocketDisablesUntilNextLaunch(t *testing.T) {
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(filepath.Join(t.TempDir(), "missing.sock"))
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	first := m.imeReporter.error()
	if first == nil || m.imeFocused || m.imeReporter.authorized() {
		t.Fatal("missing service masqueraded as applied protection")
	}
	for _, msg := range []tea.Msg{tea.ResumeMsg{}, tea.FocusMsg{},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("abc")}} {
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	if !m.filtering || m.filter != "abc" || m.imeReporter.error() != first ||
		m.imeReporter.conn != nil || m.imeReporter.state != "" || m.imeFocused {
		t.Fatal("disabled reporter swallowed text, retried, or retained active intent")
	}
	if _, ok := IMEFilter(m, tea.SuspendMsg{}).(tea.SuspendMsg); !ok {
		t.Fatal("disabled reporter blocked suspension")
	}
	if err := m.EnableTUIIME(); err != nil || m.imeReporter.error() != first {
		t.Fatal("explicit enable retried the failed reporter")
	}
	if err := m.CloseIME(); err != nil {
		t.Fatalf("disabled reporter leaked a close error: %v", err)
	}
}

func TestIMECloseRejectionIsOptional(t *testing.T) {
	path := serveTestIME(t, func(id uint64, request imeRequest) (imeResponse, bool) {
		if request.Op == "close" {
			return imeResponse{Generation: 1, Error: "UNKNOWN"}, true
		}
		return appliedTestIMEACK(id), true
	})
	m := New(config.Default(), "", nil, nil)
	m.imeReporter = newIMEReporter(path)
	if err := m.EnableTUIIME(); err != nil {
		t.Fatal(err)
	}
	if err := m.CloseIME(); err != nil {
		t.Fatalf("optional close leaked an error: %v", err)
	}
	if m.imeReporter.error() == nil || m.imeReporter.authorized() || m.imeReporter.conn != nil ||
		m.imeReporter.state != "" {
		t.Fatal("rejected close retained or fabricated active intent")
	}
}
