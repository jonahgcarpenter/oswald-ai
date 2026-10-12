// Package process tracks profile-scoped background shell processes.
//
// Each profile owns one Manager bound to its profile root. Process output
// and metadata persist under <root>/cache/processes/<id>/ with private
// permissions so completed results survive agent turns (but not necessarily
// restarts for running processes). Completed records are retained 7 days,
// newest 64 per profile. Running entries exist only in memory; a record
// found on disk with status running but no live entry belongs to a dead
// owner and is marked killed on first observation.
package process

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status describes a tracked process.
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusKilled    Status = "killed"
)

// Info is the persisted + live summary of one process.
type Info struct {
	ID             string `json:"id"`
	PID            int    `json:"pid"`
	CommandPreview string `json:"command_preview"`
	Status         Status `json:"status"`
	ExitCode       int    `json:"exit_code"`
	Persist        bool   `json:"persist"`
	Owner          string `json:"owner,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
	CreatedUnix    int64  `json:"created_unix"`
	UpdatedUnix    int64  `json:"updated_unix"`
	Note           string `json:"note,omitempty"`
}

type liveProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	file  *os.File
	mu    sync.Mutex
	done  chan struct{}
	info  Info
}

// Manager owns background processes for one profile root.
type Manager struct {
	root     string
	mu       sync.Mutex
	running  map[string]*liveProc
	handoffs map[string]int
}

var globalMu sync.Mutex
var globalManagers = make(map[*Manager]struct{})
var managersByRoot = make(map[string]*Manager)

// Shared returns the manager for a profile root, creating it on first use
// so tool handlers and command handlers operate on one instance.
func Shared(profileRoot string) *Manager {
	abs := strings.TrimSpace(profileRoot)
	if abs != "" {
		if resolved, err := filepath.Abs(abs); err == nil {
			abs = resolved
		}
	}
	globalMu.Lock()
	defer globalMu.Unlock()
	if m, ok := managersByRoot[abs]; ok {
		return m
	}
	m := &Manager{root: abs, running: make(map[string]*liveProc), handoffs: make(map[string]int)}
	globalManagers[m] = struct{}{}
	managersByRoot[abs] = m
	return m
}

// StopAll terminates non-persistent processes across all managers.
func StopAll() {
	globalMu.Lock()
	managers := make([]*Manager, 0, len(globalManagers))
	for m := range globalManagers {
		managers = append(managers, m)
	}
	globalMu.Unlock()
	for _, m := range managers {
		m.Stop()
	}
}

// NewManager binds a manager to a profile root. Prefer Shared so command
// and tool handlers use one instance per root. An empty root fails closed.
func NewManager(profileRoot string) *Manager {
	return Shared(profileRoot)
}

func (m *Manager) baseDir() (string, error) {
	if m == nil || m.root == "" {
		return "", errors.New("process manager is unavailable")
	}
	return filepath.Join(m.root, "cache", "processes"), nil
}

func (m *Manager) procDir(id string) (string, error) {
	base, err := m.baseDir()
	if err != nil {
		return "", err
	}
	if !validID(id) {
		return "", errors.New("unknown session")
	}
	return filepath.Join(base, id), nil
}

func validID(id string) bool {
	if len(id) < 4 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

func commandPreview(command string) string {
	r := []rune(strings.Join(strings.Fields(command), " "))
	if len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return string(r)
}

// Spawn starts a tracked background shell process.
func (m *Manager) Spawn(ownerRequest, command, workdir, scratch string, timeoutSecs int) (Info, error) {
	if m == nil || m.root == "" {
		return Info{}, errors.New("process manager is unavailable")
	}
	if strings.TrimSpace(command) == "" {
		return Info{}, errors.New("command is required")
	}
	base, err := m.baseDir()
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return Info{}, err
	}
	id := newID()
	dir := filepath.Join(base, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return Info{}, err
	}
	shell := "/bin/sh"
	if _, err := os.Stat(shell); err != nil {
		return Info{}, errors.New("shell is unavailable")
	}
	out, err := os.OpenFile(filepath.Join(dir, "output.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return Info{}, err
	}
	cmd := exec.Command(shell, "-c", command)
	cmd.Dir = workdir
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"LANG=C.UTF-8",
		"TMPDIR=" + scratch,
		"OSWALD_WORKSPACE=" + workdir,
		"OSWALD_SCRATCH=" + scratch,
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		out.Close()
		return Info{}, err
	}
	cmd.Stdout = out
	cmd.Stderr = out
	now := time.Now().Unix()
	info := Info{ID: id, CommandPreview: commandPreview(command), Status: StatusRunning, ExitCode: 0, Owner: ownerRequest, CreatedUnix: now, UpdatedUnix: now}
	if err := writeMeta(dir, info); err != nil {
		out.Close()
		return Info{}, err
	}
	if err := cmd.Start(); err != nil {
		out.Close()
		return Info{}, errors.New("command failed to start")
	}
	live := &liveProc{cmd: cmd, stdin: stdin, file: out, done: make(chan struct{}), info: info}
	live.info.PID = cmd.Process.Pid
	m.mu.Lock()
	m.running[id] = live
	outcome := live.info
	m.mu.Unlock()
	if err := writeMeta(dir, outcome); err != nil {
		// Best effort; the watcher will still finalize.
	}
	go m.watch(id, dir, live, timeoutSecs)
	m.pruneAsync()
	return outcome, nil
}

func (m *Manager) watch(id, dir string, live *liveProc, timeoutSecs int) {
	defer close(live.done)
	defer live.file.Close()
	if timeoutSecs > 0 {
		timer := time.NewTimer(time.Duration(timeoutSecs) * time.Second)
		defer timer.Stop()
		done := make(chan struct{})
		go func() {
			select {
			case <-done:
			case <-timer.C:
				live.mu.Lock()
				if live.cmd.Process != nil {
					_ = live.cmd.Process.Kill()
				}
				live.mu.Unlock()
			}
		}()
		_ = live.cmd.Wait()
		close(done)
	} else {
		_ = live.cmd.Wait()
	}
	exit := 0
	killed := false
	if live.cmd.ProcessState != nil {
		exit = live.cmd.ProcessState.ExitCode()
	} else {
		killed = true
		exit = -1
	}
	status := StatusCompleted
	if killed {
		status = StatusKilled
	}
	m.mu.Lock()
	live.info.Status = status
	live.info.ExitCode = exit
	live.info.UpdatedUnix = time.Now().Unix()
	final := live.info
	m.mu.Unlock()
	_ = writeMeta(dir, final)
}

// Resolve expands a session_id prefix to a full id.
func (m *Manager) Resolve(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", errors.New("session_id is required")
	}
	ids, err := m.knownIDs()
	if err != nil {
		return "", err
	}
	var match string
	count := 0
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			match = id
			count++
		}
	}
	if count == 0 {
		return "", errors.New("unknown session")
	}
	if count > 1 {
		return "", fmt.Errorf("ambiguous session prefix (%d matches)", count)
	}
	return match, nil
}

func (m *Manager) knownIDs() ([]string, error) {
	base, err := m.baseDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() && validID(e.Name()) {
			seen[e.Name()] = true
		}
	}
	m.mu.Lock()
	for id := range m.running {
		seen[id] = true
	}
	m.mu.Unlock()
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Load returns the current info, marking orphaned running records killed.
func (m *Manager) Load(id string) (Info, error) {
	full, err := m.Resolve(id)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	live, ok := m.running[full]
	m.mu.Unlock()
	if ok {
		m.mu.Lock()
		info := live.info
		m.mu.Unlock()
		return info, nil
	}
	dir, err := m.procDir(full)
	if err != nil {
		return Info{}, err
	}
	info, err := readMeta(dir)
	if err != nil {
		return Info{}, errors.New("unknown session")
	}
	if info.Status == StatusRunning {
		info.Status = StatusKilled
		info.ExitCode = -1
		info.Note = "owner ended before completion"
		_ = writeMeta(dir, info)
	}
	return info, nil
}

// List returns newest-first infos, pruning retention first.
func (m *Manager) List() ([]Info, error) {
	m.prune()
	ids, err := m.knownIDs()
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(ids))
	for _, id := range ids {
		info, err := m.Load(id)
		if err != nil {
			continue
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].UpdatedUnix == infos[j].UpdatedUnix {
			return infos[i].ID > infos[j].ID
		}
		return infos[i].UpdatedUnix > infos[j].UpdatedUnix
	})
	return infos, nil
}

// Tail returns the last n bytes of raw output.
func (m *Manager) Tail(id string, n int) (string, error) {
	full, err := m.Resolve(id)
	if err != nil {
		return "", err
	}
	dir, err := m.procDir(full)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(dir, "output.log"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if n > 0 && len(data) > n {
		data = data[len(data)-n:]
		// Cut to a line boundary.
		if i := indexByte(data, '\n'); i >= 0 && i < 512 {
			data = data[i+1:]
		}
	}
	return string(data), nil
}

// LogLines returns offset/limit line pagination over raw output.
func (m *Manager) LogLines(id string, offset, limit int) ([]string, int, error) {
	full, err := m.Resolve(id)
	if err != nil {
		return nil, 0, err
	}
	dir, err := m.procDir(full)
	if err != nil {
		return nil, 0, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "output.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	lines := splitLines(string(data))
	if offset >= len(lines) {
		return nil, len(lines), nil
	}
	end := offset + limit
	if end > len(lines) {
		end = len(lines)
	}
	return lines[offset:end], len(lines), nil
}

// Wait blocks until completion or timeout seconds.
func (m *Manager) Wait(id string, timeoutSecs int) (Info, error) {
	full, err := m.Resolve(id)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	live, ok := m.running[full]
	m.mu.Unlock()
	if !ok {
		return m.Load(full)
	}
	if timeoutSecs <= 0 {
		<-live.done
		return m.Load(full)
	}
	select {
	case <-live.done:
		return m.Load(full)
	case <-time.After(time.Duration(timeoutSecs) * time.Second):
		return Info{}, errors.New("wait timed out")
	}
}

// Kill terminates a running process.
func (m *Manager) Kill(id string) (Info, error) {
	full, err := m.Resolve(id)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	live, ok := m.running[full]
	m.mu.Unlock()
	if !ok {
		return m.Load(full)
	}
	live.mu.Lock()
	proc := live.cmd.Process
	live.mu.Unlock()
	if proc != nil {
		_ = proc.Kill()
	}
	select {
	case <-live.done:
	case <-time.After(10 * time.Second):
	}
	return m.Load(full)
}

// Write appends raw bytes to stdin.
func (m *Manager) Write(id, data string) error {
	full, err := m.Resolve(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	live, ok := m.running[full]
	m.mu.Unlock()
	if !ok {
		return errors.New("process is not running")
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	_, err = io.WriteString(live.stdin, data)
	return err
}

// Submit appends data plus Enter to stdin.
func (m *Manager) Submit(id, data string) error {
	return m.Write(id, data+"\n")
}

// Close sends EOF to stdin.
func (m *Manager) Close(id string) error {
	full, err := m.Resolve(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	live, ok := m.running[full]
	m.mu.Unlock()
	if !ok {
		return errors.New("process is not running")
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	return live.stdin.Close()
}

// Handoff transfers ownership to the parent scope. Cap 3 per owner request.
func (m *Manager) Handoff(id, ownerRequest, purpose string) (Info, error) {
	if strings.TrimSpace(purpose) == "" {
		return Info{}, errors.New("handoff requires a purpose sentence")
	}
	full, err := m.Resolve(id)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	if m.handoffs[ownerRequest] >= 3 {
		m.mu.Unlock()
		return Info{}, errors.New("handoff cap reached for this request")
	}
	m.handoffs[ownerRequest]++
	live, ok := m.running[full]
	m.mu.Unlock()
	dir, err := m.procDir(full)
	if err != nil {
		return Info{}, err
	}
	if ok {
		m.mu.Lock()
		live.info.Persist = true
		live.info.Owner = "handoff"
		live.info.Purpose = strings.TrimSpace(purpose)
		info := live.info
		m.mu.Unlock()
		_ = writeMeta(dir, info)
		return info, nil
	}
	info, err := readMeta(dir)
	if err != nil {
		return Info{}, errors.New("unknown session")
	}
	info.Persist = true
	info.Owner = "handoff"
	info.Purpose = strings.TrimSpace(purpose)
	_ = writeMeta(dir, info)
	return info, nil
}

// SetPersist marks a record persistent across teardown.
func (m *Manager) SetPersist(id string, persist bool) error {
	full, err := m.Resolve(id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	live, ok := m.running[full]
	m.mu.Unlock()
	dir, err := m.procDir(full)
	if err != nil {
		return err
	}
	if ok {
		m.mu.Lock()
		live.info.Persist = persist
		info := live.info
		m.mu.Unlock()
		return writeMeta(dir, info)
	}
	info, err := readMeta(dir)
	if err != nil {
		return errors.New("unknown session")
	}
	info.Persist = persist
	return writeMeta(dir, info)
}

// Stop kills non-persistent processes and joins them. Persistent entries
// keep their on-disk records for later polling.
func (m *Manager) Stop() {
	m.mu.Lock()
	lives := make(map[string]*liveProc, len(m.running))
	for id, live := range m.running {
		lives[id] = live
	}
	m.mu.Unlock()
	for _, live := range lives {
		m.mu.Lock()
		persist := live.info.Persist
		m.mu.Unlock()
		if persist {
			continue
		}
		live.mu.Lock()
		if live.cmd.Process != nil {
			_ = live.cmd.Process.Kill()
		}
		live.mu.Unlock()
	}
	for _, live := range lives {
		select {
		case <-live.done:
		case <-time.After(10 * time.Second):
		}
	}
}

func (m *Manager) pruneAsync() { m.prune() }

func (m *Manager) prune() {
	base, err := m.baseDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	type candidate struct {
		id      string
		updated int64
	}
	var completed []candidate
	now := time.Now().Unix()
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		info, err := readMeta(filepath.Join(base, e.Name()))
		if err != nil {
			continue
		}
		m.mu.Lock()
		_, live := m.running[e.Name()]
		m.mu.Unlock()
		if live || info.Status == StatusRunning {
			continue
		}
		if now-info.UpdatedUnix > 7*24*3600 {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
			continue
		}
		completed = append(completed, candidate{id: e.Name(), updated: info.UpdatedUnix})
	}
	if len(completed) > 64 {
		sort.Slice(completed, func(i, j int) bool { return completed[i].updated < completed[j].updated })
		for _, c := range completed[:len(completed)-64] {
			_ = os.RemoveAll(filepath.Join(base, c.id))
		}
	}
}

func writeMeta(dir string, info Info) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meta-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, "meta.json"))
}

func readMeta(dir string) (Info, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, err
	}
	return info, nil
}

func splitLines(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
