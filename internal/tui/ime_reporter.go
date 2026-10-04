package tui

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	tea "github.com/charmbracelet/bubbletea"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// imeReporter owns either a local daemon lease or a Herdr intent stream, never
// an input-source snapshot. Model copies and the Program filter share its serial
// lifecycle and first transport failure.
type imeReporter struct {
	mu              sync.Mutex
	path            string
	target          map[string]string
	conn            net.Conn
	reader          *bufio.Reader
	session         string
	generation      uint64
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

func newIMEReporter(path string) *imeReporter { return &imeReporter{path: path} }

func configuredIMEReporter() (*imeReporter, error) {
	if marker, present := os.LookupEnv("HERDR_IME_INTENT"); present {
		if marker != "1" {
			return nil, fmt.Errorf("HERDR_IME_INTENT_UNSUPPORTED: marker %q", marker)
		}
		pane, popup := os.Getenv("HERDR_PANE_ID"), os.Getenv("HERDR_IME_POPUP_TERMINAL_ID")
		if os.Getenv("HERDR_ENV") != "1" || (pane == "") == (popup == "") {
			return nil, errors.New("HERDR_IME_INTENT_INVALID: require HERDR_ENV=1 and exactly one terminal identity")
		}
		path := os.Getenv("HERDR_SOCKET_PATH")
		if err := validateIMESocket(path); err != nil {
			return nil, fmt.Errorf("HERDR_IME_INTENT_INVALID: %w", err)
		}
		target := map[string]string{"pane_id": pane}
		if popup != "" {
			target = map[string]string{"popup_terminal_id": popup}
		}
		return &imeReporter{path: path, target: target}, nil
	}
	path, err := localIMESocket()
	if err != nil {
		return nil, err
	}
	return newIMEReporter(path), nil
}

func localIMESocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local/state/infra-as-code/ime-control/run/control.sock"), nil
}

// exchange runs under mu. An inactive direct ACK is a normal foreground refusal;
// protocol/transport failures permanently disable this reporter, without replay.
func (r *imeReporter) exchange(request imeRequest) (scope string, err error) {
	defer func() {
		if err != nil {
			err = r.fail(err)
		}
	}()
	if r.conn == nil {
		if err := validateIMESocket(r.path); err != nil {
			return "", err
		}
		conn, err := net.DialTimeout("unix", r.path, time.Second)
		if err != nil {
			return "", fmt.Errorf("connect: %w", err)
		}
		r.conn = conn
		r.reader = bufio.NewReaderSize(conn, 4096)
		if r.target != nil {
			if err := r.openIntentStream(); err != nil {
				return "", err
			}
		}
	}
	if err := r.conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return "", err
	}
	line, err := r.rpc(request)
	if err != nil {
		return "", err
	}
	var response struct {
		OK         *bool   `json:"ok"`
		Generation *uint64 `json:"generation"`
		Session    *string `json:"session"`
		Scope      *string `json:"scope"`
		Error      *string `json:"error"`
	}
	if err := decodeIMEFrame(line, &response); err != nil {
		return "", fmt.Errorf("invalid IME ACK: %w", err)
	}
	if response.OK == nil || response.Generation == nil || *response.Generation == 0 ||
		*response.Generation < r.generation {
		return "", errors.New("IME service returned incomplete or regressed ACK")
	}
	if !*response.OK {
		if response.Error == nil || *response.Error == "" || response.Session != nil || response.Scope != nil {
			return "", errors.New("IME service returned malformed rejection")
		}
		return "", fmt.Errorf("IME service rejected %s: %s", request.Op, *response.Error)
	}
	if response.Session == nil || *response.Session == "" || response.Scope == nil || response.Error != nil ||
		(r.session != "" && *response.Session != r.session) {
		return "", errors.New("IME service returned incomplete or changed-session ACK")
	}
	scope = *response.Scope
	if r.target != nil {
		if scope != "recorded" {
			return "", errors.New("Herdr ACK must have scope recorded")
		}
	} else if scope != "applied" && scope != "inactive" && !(scope == "pending" && request.Op == "blur") {
		return "", errors.New("invalid direct IME ACK scope")
	}
	r.session, r.generation = *response.Session, *response.Generation
	return scope, nil
}

func decodeIMEFrame(frame []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(frame))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing IME frame data")
	}
	return nil
}

func (r *imeReporter) rpc(request any) ([]byte, error) {
	frame, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	frame = append(frame, '\n')
	if len(frame) > 4096 {
		return nil, errors.New("IME request exceeds 4096 bytes")
	}
	n, err := r.conn.Write(frame)
	if err != nil || n != len(frame) {
		if err == nil {
			err = errors.New("short IME request write")
		}
		return nil, fmt.Errorf("send: %w", err)
	}
	line, err := r.reader.ReadSlice('\n')
	if err != nil || len(line) > 4096 {
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			err = errors.New("IME ACK exceeds 4096 bytes")
		}
		return nil, fmt.Errorf("receive IME ACK: %w", err)
	}
	return line, nil
}

func (r *imeReporter) openIntentStream() error {
	if err := r.conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	request := struct {
		ID     string            `json:"id"`
		Method string            `json:"method"`
		Params map[string]string `json:"params"`
	}{"ime:open", "pane.input_intent.stream", r.target}
	line, err := r.rpc(request)
	if err != nil {
		return fmt.Errorf("open Herdr intent stream: %w", err)
	}
	var response struct {
		ID     string `json:"id"`
		Result *struct {
			Type       string  `json:"type"`
			Session    string  `json:"session"`
			Generation *uint64 `json:"generation"`
		} `json:"result"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := decodeIMEFrame(line, &response); err != nil {
		return fmt.Errorf("invalid Herdr stream open ACK: %w", err)
	}
	if response.ID == "ime:open" && response.Result == nil && response.Error != nil && response.Error.Code != "" {
		return fmt.Errorf("Herdr stream open rejected: %s: %s", response.Error.Code, response.Error.Message)
	}
	if response.ID != "ime:open" || response.Result == nil ||
		response.Result.Type != "pane_input_intent_stream_opened" || response.Result.Session == "" ||
		response.Result.Generation == nil || *response.Result.Generation == 0 || response.Error != nil {
		return errors.New("Herdr rejected or returned an incomplete intent stream open ACK")
	}
	r.session, r.generation = response.Result.Session, *response.Result.Generation
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

func (r *imeReporter) fail(err error) error {
	r.drop()
	r.state = ""
	r.suspended = false
	r.suspendInactive = false
	if r.failure == nil {
		r.failure = fmt.Errorf("IME reporting failed: %w", err)
	}
	return r.failure
}

// episode starts a genuine focus/startup episode. Suspended parent streams
// resume explicitly; background state updates never do.
func (r *imeReporter) episode(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.episodeLocked(state, false)
}

// key revalidates a direct mode lease even when a compositor-only pause never
// reached the TUI as BlurMsg. Resume preserves an existing command snapshot.
func (r *imeReporter) key(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.episodeLocked(state, true)
}

func (r *imeReporter) episodeLocked(state string, keyboard bool) error {
	if r.failure != nil {
		return r.failure
	}
	if r.closed {
		return nil
	}
	if r.active && r.target != nil {
		if r.reader.Buffered() != 0 {
			return r.fail(errors.New("unsolicited Herdr intent stream data"))
		}
		if err := probeIMESocket(r.conn); err != nil {
			return r.fail(err)
		}
		return r.reportLocked(state)
	}
	op := "activate"
	if (keyboard && r.target == nil && r.conn != nil) || (r.suspended && !r.suspendInactive && r.conn != nil) {
		op = "resume"
	}
	request := imeRequest{Op: op, State: state}
	if op == "activate" {
		request.Policy = "mode"
	}
	scope, err := r.exchange(request)
	if err != nil {
		return err
	}
	r.state = state
	r.active = scope == "applied" || scope == "recorded"
	r.suspended = false
	r.suspendInactive = false
	return nil
}

func (r *imeReporter) report(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	return r.reportLocked(state)
}

func (r *imeReporter) reportLocked(state string) error {
	if r.closed || r.suspended || !r.active || r.state == state {
		return nil
	}
	scope, err := r.exchange(imeRequest{Op: "state", State: state})
	if err != nil {
		return err
	}
	r.state = state
	r.active = scope == "applied" || scope == "recorded"
	return nil
}

func (r *imeReporter) authorized() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active && !r.suspended && !r.closed && r.failure == nil
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
	_, err := r.exchange(imeRequest{Op: "blur"})
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
	if r.conn != nil {
		if _, err := r.exchange(imeRequest{Op: "suspend"}); err != nil {
			return err
		}
	}
	r.suspended = true
	r.active = false
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
		_, err = r.exchange(imeRequest{Op: "close"})
	}
	r.drop()
	return err
}

// EnableTUIIME selects Herdr transport before ordinary local GUI exclusions.
// Herdr records intent only; no enabled pane contacts the server host's daemon.
func (m *Model) EnableTUIIME() error {
	if m.imeReporter == nil {
		reporter, err := configuredIMEReporter()
		if err != nil {
			m.imeReporter = &imeReporter{failure: err}
			m.imeFocused = false
			return nil
		}
		m.imeReporter = reporter
	}
	_ = m.imeReporter.episode(m.imeState())
	m.imeFocused = m.imeReporter.authorized()
	return nil
}

// CloseIME waits for the lifecycle close ACK; callers invoke it before os.Exit.
func (m *Model) CloseIME() error {
	if m.imeReporter == nil {
		return nil
	}
	_ = m.imeReporter.close()
	return nil
}

func (m Model) imeState() string {
	if m.filtering || m.shellPrompting || m.aiPrompting || m.editingOpenCmd {
		return "text"
	}
	return "command"
}

// IMEFilter suspends the reporter before Bubble Tea handles SuspendMsg internally;
// this runs before Model.Update can see the suspension. A failed release disables
// only the optional reporter; native terminal suspension still proceeds.
func IMEFilter(model tea.Model, msg tea.Msg) tea.Msg {
	if _, suspended := msg.(tea.SuspendMsg); !suspended {
		return msg
	}
	m, ok := model.(Model)
	if !ok || m.imeReporter == nil {
		return msg
	}
	_ = m.imeReporter.suspend()
	return msg
}
