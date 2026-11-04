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

// imeReporter owns either a local daemon lease or an hc terminal label, never
// an input-source snapshot. Model copies and the Program filter share its serial
// lifecycle and first transport failure.
type imeReporter struct {
	mu         sync.Mutex
	path       string
	terminal   string // HC_TERMINAL_ID; non-empty selects the hc transport
	held       bool   // hc transport: the label currently claims focus intent
	rpcID      uint64
	conn       net.Conn
	reader     *bufio.Reader
	session    string
	generation uint64
	state      string
	active     bool
	focusLost  bool // explicit blur, independent of lease authorization
	suspended  bool
	closed     bool
	failure    error
}

type imeSocketIdentity struct{ dev, ino uint64 }

type imeRequest struct {
	Op     string `json:"op"`
	State  string `json:"state,omitempty"`
	Policy string `json:"policy,omitempty"`
}

func newIMEReporter(path string) *imeReporter { return &imeReporter{path: path} }

// configuredIMEReporter selects the hc transport when either hc variable is
// present; a partial launch is an error so callers disable without fallback.
func configuredIMEReporter() (*imeReporter, error) {
	terminal, socket := os.Getenv("HC_TERMINAL_ID"), os.Getenv("HC_TERMINAL_SOCKET")
	if terminal != "" || socket != "" {
		if terminal == "" || socket == "" {
			return nil, errors.New("HC_TERMINAL_INVALID: require HC_TERMINAL_ID and HC_TERMINAL_SOCKET")
		}
		if _, err := validateHCSocket(socket); err != nil {
			return nil, fmt.Errorf("HC_TERMINAL_INVALID: %w", err)
		}
		return &imeReporter{path: socket, terminal: terminal}, nil
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
		if err := r.connect(); err != nil {
			return "", err
		}
	}
	if r.terminal != "" {
		return r.labelExchange(request)
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
	if scope != "applied" && scope != "inactive" && !(scope == "pending" && request.Op == "blur") {
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

func (r *imeReporter) connect() error {
	var identity imeSocketIdentity
	var err error
	if r.terminal != "" {
		identity, err = validateHCSocket(r.path)
	} else {
		err = validateIMESocket(r.path)
	}
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", r.path, time.Second)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if r.terminal != "" {
		// The path may be swapped between validation and connect.
		if after, err := validateHCSocket(r.path); err != nil || after != identity {
			_ = conn.Close()
			return errors.New("hc socket identity changed during connect")
		}
	}
	r.conn = conn
	r.reader = bufio.NewReaderSize(conn, 4096)
	return nil
}

// labelExchange maps a lifecycle request onto the imekit.intent terminal label
// and returns the synthetic recorded ACK scope; see imekit docs/protocol.md.
func (r *imeReporter) labelExchange(request imeRequest) (string, error) {
	write, value := true, ""
	switch request.Op {
	case "enter":
		value, r.held = "command", true
	case "activate", "resume":
		value, r.held = request.State, true
	case "state":
		value, write = request.State, r.held
	case "blur", "suspend":
		value, r.held = "idle", false
	case "close":
		r.held = false
	default:
		return "", fmt.Errorf("unsupported hc lifecycle %q", request.Op)
	}
	if value != "" && value != "command" && value != "text" && value != "idle" {
		return "", fmt.Errorf("invalid hc intent state %q", value)
	}
	if write {
		if value != "" {
			pgid, err := imeProcessGroup()
			if err != nil {
				return "", err
			}
			value = fmt.Sprintf("1:%d:%s", pgid, value)
		}
		if err := r.writeLabel(value); err != nil {
			return "", err
		}
	}
	r.generation++
	r.session = "hc:" + r.terminal
	return "recorded", nil
}

func (r *imeReporter) writeLabel(value string) error {
	if err := r.conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		return err
	}
	r.rpcID++
	type params struct {
		Terminal string            `json:"terminal"`
		Labels   map[string]string `json:"labels"`
	}
	request := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      uint64 `json:"id"`
		Method  string `json:"method"`
		Params  params `json:"params"`
	}{"2.0", r.rpcID, "terminal.label", params{r.terminal, map[string]string{"imekit.intent": value}}}
	line, err := r.rpc(request)
	if err != nil {
		return fmt.Errorf("hc terminal.label: %w", err)
	}
	var response struct {
		ID     *uint64         `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &response); err != nil {
		return fmt.Errorf("invalid hc reply: %w", err)
	}
	if len(response.Error) != 0 {
		return fmt.Errorf("hc terminal.label rejected: %s", response.Error)
	}
	if response.ID == nil || *response.ID != r.rpcID {
		return errors.New("hc reply id mismatch")
	}
	var result struct {
		Labels map[string]json.RawMessage `json:"labels"`
	}
	if err := json.Unmarshal(response.Result, &result); err != nil || result.Labels == nil {
		return errors.New("hc reply lacks result labels")
	}
	raw, present := result.Labels["imekit.intent"]
	var got string
	if present && json.Unmarshal(raw, &got) != nil {
		return errors.New("hc reply label is not a string")
	}
	if value == "" && present || value != "" && got != value {
		return errors.New("hc reply does not confirm the written label")
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

func (r *imeReporter) fail(err error) error {
	r.drop()
	r.state = ""
	r.suspended = false
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
	r.focusLost = false
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
	if r.closed || r.suspended || r.focusLost {
		return nil
	}
	if r.active && r.terminal != "" {
		if r.reader.Buffered() != 0 {
			return r.fail(errors.New("unsolicited hc reply data"))
		}
		if err := probeIMESocket(r.conn); err != nil {
			return r.fail(err)
		}
		return r.reportLocked(state)
	}
	op := "activate"
	if keyboard && r.terminal == "" && r.conn != nil {
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
	r.focusLost = true
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
	if r.conn != nil {
		if _, err := r.exchange(imeRequest{Op: "suspend"}); err != nil {
			return err
		}
	}
	r.suspended = true
	r.active = false
	return nil
}

// returned samples only this app's mode after its terminal handoff completes.
// A fresh claim handles a child's delayed close; the transport arbitrates focus.
func (r *imeReporter) returned(state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if r.closed || !r.suspended {
		return nil
	}
	r.suspended = false
	return r.episodeLocked(state, false)
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

// EnableTUIIME selects the hc transport before ordinary local GUI exclusions.
// hc records intent only; an hc terminal never contacts the direct daemon.
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
