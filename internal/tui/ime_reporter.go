package tui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// imeReporter owns one daemon lease, not the input source itself. Model copies
// share its lifecycle and first failure; the filter and Update serialize RPCs
// through the same mutex.
type imeReporter struct {
	mu              sync.Mutex
	path            string
	conn            net.Conn
	reader          *bufio.Reader
	state           string
	active          bool
	suspended       bool
	suspendInactive bool
	closed          bool
	failure         error
}

type imeRequest struct {
	Op     string `json:"op"`
	State  string `json:"state,omitempty"`
	Policy string `json:"policy,omitempty"`
}

type imeResponse struct {
	OK         bool   `json:"ok"`
	Generation uint64 `json:"generation"`
	Session    string `json:"session"`
	Error      string `json:"error"`
}

func newIMEReporter(path string) *imeReporter { return &imeReporter{path: path} }

func localIMESocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/state/infra-as-code/ime-control/run/control.sock"), nil
}

// exchange is called under mu and waits for a durable-transition ACK before
// keyboard dispatch continues. A failed RPC drops the connection and is fatal:
// reconnecting cannot silently claim that the rejected mode was protected.
func (r *imeReporter) exchange(request imeRequest) (err error) {
	defer func() {
		if err != nil {
			r.drop()
			if r.failure == nil {
				r.failure = fmt.Errorf("IME mode unprotected: %w", err)
			}
			err = r.failure
		}
	}()
	if r.conn == nil {
		conn, err := net.DialTimeout("unix", r.path, 2*time.Second)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		r.conn = conn
		r.reader = bufio.NewReaderSize(conn, 4096)
	}
	if err := r.conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	frame, err := json.Marshal(request)
	if err != nil {
		return err
	}
	frame = append(frame, '\n')
	if len(frame) > 4096 {
		return errors.New("IME request exceeds 4096 bytes")
	}
	n, err := r.conn.Write(frame)
	if err != nil || n != len(frame) {
		if err == nil {
			err = errors.New("short IME request write")
		}
		return fmt.Errorf("send: %w", err)
	}
	line, err := r.reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 {
		if err == nil {
			err = errors.New("IME ACK exceeds 4096 bytes")
		}
		return fmt.Errorf("receive IME ACK: %w", err)
	}
	var response imeResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("invalid IME ACK: %w", err)
	}
	if !response.OK {
		return fmt.Errorf("IME service rejected %s: %s", request.Op, response.Error)
	}
	if response.Generation == 0 || response.Session == "" {
		return errors.New("IME service returned incomplete ACK")
	}
	return nil
}

func (r *imeReporter) drop() {
	if r.conn != nil {
		_ = r.conn.Close()
	}
	r.conn = nil
	r.reader = nil
	r.active = false
}

func (r *imeReporter) report(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if r.closed || r.suspended {
		return nil
	}
	if !r.active {
		if err := r.exchange(imeRequest{Op: "activate", State: state, Policy: "mode"}); err != nil {
			return err
		}
		r.active = true
	} else if r.state != state {
		if err := r.exchange(imeRequest{Op: "state", State: state}); err != nil {
			return err
		}
	} else {
		return nil
	}
	r.state = state
	return nil
}

func (r *imeReporter) blur() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if !r.active || r.closed || r.suspended {
		return nil
	}
	err := r.exchange(imeRequest{Op: "blur"})
	r.active = false
	return err
}

func (r *imeReporter) suspend() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if r.closed || r.suspended {
		return nil
	}
	r.suspendInactive = !r.active
	if r.active {
		if err := r.exchange(imeRequest{Op: "suspend"}); err != nil {
			return err
		}
	}
	r.suspended = true
	r.active = false
	return nil
}

func (r *imeReporter) resume(state string, focused bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if r.closed || !r.suspended {
		return nil
	}
	if !focused {
		// A resumed but still blurred TUI must not reclaim the foreground owner.
		r.suspended = false
		r.suspendInactive = false
		return nil
	}
	op := "resume"
	if r.conn == nil || r.suspendInactive {
		op = "activate"
	}
	request := imeRequest{Op: op, State: state}
	if op == "activate" {
		request.Policy = "mode"
	}
	if err := r.exchange(request); err != nil {
		return err
	}
	r.state = state
	r.active = true
	r.suspended = false
	r.suspendInactive = false
	return nil
}

func (r *imeReporter) error() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

func (r *imeReporter) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var err error
	if r.conn != nil {
		err = r.exchange(imeRequest{Op: "close"})
	}
	r.drop()
	return err
}

// EnableTUIIME obtains the initial ACK before the interactive keyboard loop.
// The caller must restrict this to a local GUI terminal's actual TUI path.
func (m *Model) EnableTUIIME() error {
	if m.imeReporter != nil {
		return m.imeReporter.report(m.imeState())
	}
	path, err := localIMESocket()
	if err != nil {
		return err
	}
	m.imeReporter = newIMEReporter(path)
	m.imeFocused = true
	return m.imeReporter.report(m.imeState())
}

// IMEFailure survives both value Model copies and Program filter failures.
func (m *Model) IMEFailure() error {
	if m.imeReporter == nil {
		return nil
	}
	return m.imeReporter.error()
}

// CloseIME waits for release ACK; callers must invoke it before os.Exit.
func (m *Model) CloseIME() error {
	if m.imeReporter == nil {
		return nil
	}
	return m.imeReporter.close()
}

func (m Model) imeState() string {
	if m.filtering || m.shellPrompting || m.aiPrompting || m.editingOpenCmd {
		return "text"
	}
	return "command"
}

// IMEFilter releases the lease before Bubble Tea handles SuspendMsg internally;
// this runs before Model.Update can see the suspension. Failure is shared, not
// stored in the filter's value copy, and replaces suspension with TUI shutdown.
func IMEFilter(model tea.Model, msg tea.Msg) tea.Msg {
	if _, suspended := msg.(tea.SuspendMsg); !suspended {
		return msg
	}
	m, ok := model.(Model)
	if !ok || m.imeReporter == nil {
		return msg
	}
	if err := m.imeReporter.suspend(); err != nil {
		return tea.QuitMsg{}
	}
	return msg
}
