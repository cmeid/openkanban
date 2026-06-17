package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/techdufus/openkanban/internal/board"
	"github.com/techdufus/openkanban/internal/config"
)

// StatusDir returns the agent status-file directory.
// OPENKANBAN_STATUS_DIR > ~/.cache/openkanban-status.
// The default MUST stay in sync with config.computeGuardedDirs' status
// literal (config can't import agent — would cycle).
func StatusDir() string {
	if v := os.Getenv("OPENKANBAN_STATUS_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "openkanban-status")
}

const (
	opencodeDefaultPort = 4096
	opencodeAPITimeout  = 2 * time.Second
)

type opencodeStatusResponse map[string]opencodeSessionStatus

type opencodeSessionStatus struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
	Next    int    `json:"next,omitempty"`
}

type StatusDetector struct {
	statusCache     map[string]cachedStatus
	statusCacheMu   sync.RWMutex
	cacheExpiration time.Duration
	statusDirs      []string
	httpClient      *http.Client
}

type cachedStatus struct {
	status    board.AgentStatus
	timestamp time.Time
}

func NewStatusDetector() *StatusDetector {
	homeDir, _ := os.UserHomeDir()

	return &StatusDetector{
		statusCache:     make(map[string]cachedStatus),
		cacheExpiration: 500 * time.Millisecond,
		statusDirs: []string{
			filepath.Join(homeDir, ".cache", "openkanban-status"),
		},
		httpClient: &http.Client{
			Timeout: opencodeAPITimeout,
		},
	}
}

// DetectStatus / DetectStatusWithPath / DetectStatusWithPort take a
// `sessionName` whose meaning is **the OPENKANBAN_SESSION env var
// baked into the agent at spawn time** — i.e. the name the status
// hook (`openkanban status set`) uses to choose its file path. For
// agents with a separate API-side identity (opencode's HTTP
// session.id), use the longer DetectStatusWithPortAPI form so the
// file lookup and the API lookup don't collide.
//
// The shorthand wrappers below pass `sessionName` for both — fine for
// the common case where the env var and the API id are the same
// string. The dedicated production caller (pollAgentStatusesAsync)
// keeps them separate because Claude's UUID back-fill (commit
// c718699) makes ticket.AgentSessionID diverge from the env var
// mid-session.
func (d *StatusDetector) DetectStatus(agentType, sessionName string, processRunning bool, terminalContent string) board.AgentStatus {
	return d.DetectStatusWithPortAPI(agentType, sessionName, sessionName, "", 0, processRunning, terminalContent)
}

func (d *StatusDetector) DetectStatusWithPath(agentType, sessionName, worktreePath string, processRunning bool, terminalContent string) board.AgentStatus {
	return d.DetectStatusWithPortAPI(agentType, sessionName, sessionName, worktreePath, 0, processRunning, terminalContent)
}

func (d *StatusDetector) DetectStatusWithPort(agentType, sessionName, worktreePath string, port int, processRunning bool, terminalContent string) board.AgentStatus {
	return d.DetectStatusWithPortAPI(agentType, sessionName, sessionName, worktreePath, port, processRunning, terminalContent)
}

// DetectStatusWithPortAPI separates the file-lookup key from the
// API-lookup key. `fileSessionName` matches the OPENKANBAN_SESSION
// env var (what the status hook wrote with). `apiSessionID` is the
// opencode HTTP session id (typically a UUID, distinct from the env
// var). Pass the same string for both when they're not separately
// tracked — see the wrappers above.
func (d *StatusDetector) DetectStatusWithPortAPI(agentType, fileSessionName, apiSessionID, worktreePath string, port int, processRunning bool, terminalContent string) board.AgentStatus {
	// Read the status file first so terminal markers (completed/error)
	// survive a process exit. Transient states like "working" still
	// require the process to be running, otherwise they're stale.
	fileStatus := d.readStatusFile(fileSessionName)
	if fileStatus != board.AgentNone {
		if processRunning || fileStatus == board.AgentCompleted || fileStatus == board.AgentError {
			return fileStatus
		}
	}

	if !processRunning {
		return board.AgentNone
	}

	if agentType == "opencode" && port > 0 {
		apiKey := apiSessionID
		if apiKey == "" {
			apiKey = fileSessionName
		}
		return d.queryOpencodeAPIOnPort(apiKey, port)
	}

	if terminalContent != "" {
		if status := d.detectFromTerminalContent(agentType, terminalContent); status != board.AgentNone {
			return status
		}
	}

	// Return AgentNone when status cannot be determined.
	// The UI will not show a status indicator for unknown status.
	return board.AgentNone
}

// WaitingActivityTTL is the horizon beyond which a PTY-output timestamp
// is considered stale. It no longer gates a waiting→working promotion
// (that is now decided by on-screen evidence in DetectStatusWithActivity,
// not byte-recency); it is retained as the canonical "staleness" duration
// used by tests to construct old timestamps.
const WaitingActivityTTL = 60 * time.Second

// DetectStatusWithActivity refines a file-based "waiting" using what's on
// the live PTY grid, closing the Claude Code hook gap: Notification fires
// (permission prompt) → file = "waiting" → user approves → tool runs (no
// hook for the duration) → PostToolUse finally fires. During the gap the
// file stays pinned at "waiting" even though the agent is working.
//
// The discriminator is the SCREEN, not byte-recency: a prompt Claude is
// blocked on re-renders every couple of seconds and stamps fresh activity,
// so "bytes flowed recently" cannot tell an active turn apart from a
// re-rendering prompt. Only "waiting" is refined (other states pass
// through). When the file says "waiting":
//
//   - a recognized permission prompt on screen → stays "waiting";
//   - positive evidence of an active turn (spinner / "esc to interrupt")
//     → "working";
//   - otherwise → "waiting" (the durable default: an unknown prompt type
//     or an unattached session with no grid is never mislabeled "working").
//
// lastActivity is retained in the signature (callers and the daemon's
// resolver pass it) but is no longer the promotion trigger; the IsZero
// short-circuit keeps the "no daemon report yet → trust the file" path.
//
// `fileSessionName` and `apiSessionID` are separated for the same
// reason DetectStatusWithPortAPI separates them: pollAgentStatusesAsync
// needs to look up the hook's file under OPENKANBAN_SESSION (often
// the branch name) while still using the back-filled UUID for the
// opencode HTTP API call.
func (d *StatusDetector) DetectStatusWithActivity(agentType, fileSessionName, apiSessionID, worktreePath string, port int, processRunning bool, terminalContent string, lastActivity time.Time) board.AgentStatus {
	status := d.DetectStatusWithPortAPI(agentType, fileSessionName, apiSessionID, worktreePath, port, processRunning, terminalContent)
	if status != board.AgentWaiting {
		return status
	}
	if lastActivity.IsZero() {
		return status
	}
	// A recognized approval prompt on screen → blocked on the user. This
	// is checked first so it wins even if an active-turn marker is also
	// present (it never is in Claude's real UI, but ordering guarantees it).
	if permissionPromptVisible(terminalContent) {
		return board.AgentWaiting
	}
	// The file is also pinned at "waiting" through the whole run of a tool
	// the user already approved (Notification fired, no hook until
	// PostToolUse). For a *silent* tool — e.g. a quiet `go test` in a Bash
	// tool, where Claude shows the command's output region instead of its
	// own animated spinner — no bytes flow, so the activity fallback below
	// can't rescue it and the card shows "waiting" with nothing for the
	// user to do. If the live screen shows an active-turn marker (and no
	// prompt, checked first above), the session is busy, not blocked on
	// the user → "working". This must stay ordered after the prompt guard:
	// the marker set is mutually exclusive with a prompt in Claude's real
	// UI, but the ordering is what guarantees a prompt always wins.
	if activeTurnVisible(terminalContent) {
		return board.AgentWorking
	}
	// Default: hold "waiting". Recent PTY activity alone is NOT promoted to
	// "working" — a prompt Claude is blocked on (permission box, an
	// AskUserQuestion, an idle notice) re-renders every couple of seconds
	// and stamps fresh activity, so a byte-recency override mislabels those
	// as work. The two checks above are the only ways out of "waiting": a
	// recognized prompt (→ stays waiting) or positive on-screen evidence of
	// an active turn (→ working). Anything else — an unknown prompt type we
	// don't enumerate, or a session no TUI is attached to (empty grid) —
	// fails SAFE to "waiting" rather than a misleading "working". The real
	// Notification→PostToolUse work gap is still covered because a running
	// tool shows an active-turn marker (activeTurnVisible), not just bytes.
	return status
}

// permissionPromptSignatures are substrings that appear only in an open
// interactive approval prompt — Claude Code's tool-permission box ("Do
// you want to proceed?" and its edit/create variants) and its
// keyboard-hint footer ("Esc to cancel", distinct from the running
// state's "esc to interrupt"). They are deliberately narrower than the
// keyword list in detectCodingAgentStatus: a running tool's streamed
// output won't contain them, so matching one is strong evidence the
// session is genuinely blocked on the user rather than actively working.
var permissionPromptSignatures = []string{
	"do you want to",
	"esc to cancel",
}

// permissionPromptVisible reports whether the tail of the PTY content
// shows an open approval prompt. Scoped to the last lines so a prompt
// that has already scrolled off (the tool ran and produced output)
// doesn't keep a session pinned at "waiting".
func permissionPromptVisible(content string) bool {
	if content == "" {
		return false
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	tail := strings.ToLower(strings.Join(lines, "\n"))
	for _, sig := range permissionPromptSignatures {
		if strings.Contains(tail, sig) {
			return true
		}
	}
	return false
}

// activeTurnMarkers are substrings that appear on screen only while a
// Claude turn or tool is actively running and interruptible — never on a
// permission prompt (which shows "esc to cancel") or an idle input box.
// Observed in Claude Code as of 2026-06; if the footer string drifts in a
// future version, activeTurnVisible simply stops matching and detection
// falls through to the activity fallback and then the "waiting" default —
// i.e. drift fails SAFE (a busy session shows "waiting", the original
// annoyance) and never the dangerous direction (hiding a needs-you).
var activeTurnMarkers = []string{
	"esc to interrupt",
}

// activeTurnSpinnerGlyphs are the braille frames Claude animates while
// thinking; detectCodingAgentStatus already treats them as "working".
// Listed here too because that path is skipped when the status file is
// authoritative (file=waiting short-circuits DetectStatusWithPortAPI).
var activeTurnSpinnerGlyphs = []string{
	"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏",
}

// activeTurnVisible reports whether the tail of the PTY content shows an
// active (interruptible) turn or tool. Used to lift a file-pinned
// "waiting" to "working" for an already-approved tool that emits no PTY
// bytes — but only after permissionPromptVisible has had first refusal,
// so an on-screen prompt always wins (guards the false-negative).
func activeTurnVisible(content string) bool {
	if content == "" {
		return false
	}
	lines := strings.Split(content, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	tail := strings.ToLower(strings.Join(lines, "\n"))
	for _, m := range activeTurnMarkers {
		if strings.Contains(tail, m) {
			return true
		}
	}
	for _, g := range activeTurnSpinnerGlyphs {
		if strings.Contains(tail, g) {
			return true
		}
	}
	return false
}

func (d *StatusDetector) detectFromTerminalContent(agentType, content string) board.AgentStatus {
	contentLower := strings.ToLower(content)
	lines := strings.Split(content, "\n")

	lastLines := lines
	if len(lines) > 10 {
		lastLines = lines[len(lines)-10:]
	}
	recentContent := strings.Join(lastLines, "\n")
	recentLower := strings.ToLower(recentContent)

	switch agentType {
	case "opencode", "claude", "gemini", "codex":
		return d.detectCodingAgentStatus(recentLower, contentLower)
	default:
		return d.detectGenericAgentStatus(recentLower)
	}
}

func (d *StatusDetector) detectCodingAgentStatus(recentLower, fullLower string) board.AgentStatus {
	waitingPatterns := []string{
		"waiting for",
		"do you want",
		"would you like",
		"[y/n]",
		"(y/n)",
		"press enter",
		"confirm",
		"permission",
		"approve",
		"allow",
		"accept",
		"proceed",
	}
	for _, pattern := range waitingPatterns {
		if strings.Contains(recentLower, pattern) {
			return board.AgentWaiting
		}
	}

	workingPatterns := []string{
		"thinking",
		"processing",
		"running",
		"executing",
		"writing",
		"reading",
		"searching",
		"analyzing",
		"generating",
		"fetching",
		"loading",
		"compiling",
		"building",
		"installing",
		"calling",
		"invoking",
		"━",
		"█",
		"▓",
		"●",
		"◐",
		"◓",
		"◑",
		"◒",
		"⠋",
		"⠙",
		"⠹",
		"⠸",
		"⠼",
		"⠴",
		"⠦",
		"⠧",
		"⠇",
		"⠏",
	}
	for _, pattern := range workingPatterns {
		if strings.Contains(recentLower, pattern) {
			return board.AgentWorking
		}
	}

	errorPatterns := []string{
		"error:",
		"failed:",
		"exception:",
		"rate limit",
		"quota exceeded",
		"api error",
		"timeout",
		"connection refused",
		"unauthorized",
	}
	for _, pattern := range errorPatterns {
		if strings.Contains(recentLower, pattern) {
			return board.AgentError
		}
	}

	idlePatterns := []string{
		"$/",
		"cost:",
		"tokens:",
		"messages:",
	}
	for _, pattern := range idlePatterns {
		if strings.Contains(recentLower, pattern) {
			return board.AgentIdle
		}
	}

	return board.AgentNone
}

func (d *StatusDetector) detectGenericAgentStatus(recentLower string) board.AgentStatus {
	if strings.Contains(recentLower, "error") || strings.Contains(recentLower, "failed") {
		return board.AgentError
	}
	if strings.Contains(recentLower, "...") || strings.Contains(recentLower, "processing") {
		return board.AgentWorking
	}
	return board.AgentNone
}

func (d *StatusDetector) queryOpencodeAPI(sessionID string) board.AgentStatus {
	cacheKey := "opencode:" + sessionID

	d.statusCacheMu.RLock()
	cached, exists := d.statusCache[cacheKey]
	d.statusCacheMu.RUnlock()

	if exists && time.Since(cached.timestamp) < d.cacheExpiration {
		return cached.status
	}

	url := fmt.Sprintf("http://localhost:%d/session/status", opencodeDefaultPort)
	resp, err := d.httpClient.Get(url)
	if err != nil {
		return board.AgentNone
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return board.AgentNone
	}

	var statusResp opencodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return board.AgentNone
	}

	sessionStatus, found := statusResp[sessionID]
	if !found {
		return board.AgentNone
	}

	status := d.mapOpencodeStatus(sessionStatus)

	d.statusCacheMu.Lock()
	d.statusCache[cacheKey] = cachedStatus{
		status:    status,
		timestamp: time.Now(),
	}
	d.statusCacheMu.Unlock()

	return status
}

func (d *StatusDetector) queryOpencodeAPIOnPort(sessionID string, port int) board.AgentStatus {
	cacheKey := fmt.Sprintf("opencode-port:%d:%s", port, sessionID)

	d.statusCacheMu.RLock()
	cached, exists := d.statusCache[cacheKey]
	d.statusCacheMu.RUnlock()

	if exists && time.Since(cached.timestamp) < d.cacheExpiration {
		return cached.status
	}

	url := fmt.Sprintf("http://localhost:%d/session/status", port)
	resp, err := d.httpClient.Get(url)
	if err != nil {
		return board.AgentNone
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return board.AgentNone
	}

	var statusResp opencodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return board.AgentNone
	}

	// OpenCode's /session/status only contains BUSY (and retry) sessions.
	// Look up just this session's entry — a missing entry in an otherwise
	// successful response means this session is idle, regardless of what
	// any other sessions on the same shared port are doing.
	status := board.AgentNone
	if sessionID != "" {
		if sessionStatus, found := statusResp[sessionID]; found {
			status = d.mapOpencodeStatus(sessionStatus)
		} else {
			status = board.AgentIdle
		}
	}

	d.statusCacheMu.Lock()
	d.statusCache[cacheKey] = cachedStatus{
		status:    status,
		timestamp: time.Now(),
	}
	d.statusCacheMu.Unlock()
	return status
}

func (d *StatusDetector) mapOpencodeStatus(s opencodeSessionStatus) board.AgentStatus {
	switch s.Type {
	case "busy":
		return board.AgentWorking
	case "idle":
		return board.AgentIdle
	case "retry":
		return board.AgentError
	default:
		return board.AgentNone
	}
}

// queryOpencodeStatusByDirectory queries the OpenCode API on the default port.
// This is a fallback for when no specific port is available.
func (d *StatusDetector) queryOpencodeStatusByDirectory(_ string) board.AgentStatus {
	cacheKey := "opencode-api"

	d.statusCacheMu.RLock()
	cached, exists := d.statusCache[cacheKey]
	d.statusCacheMu.RUnlock()

	if exists && time.Since(cached.timestamp) < d.cacheExpiration {
		return cached.status
	}

	statusURL := fmt.Sprintf("http://localhost:%d/session/status", opencodeDefaultPort)
	resp, err := d.httpClient.Get(statusURL)
	if err != nil {
		return board.AgentNone
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return board.AgentNone
	}

	var statusResp opencodeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return board.AgentNone
	}

	for _, sessionStatus := range statusResp {
		status := d.mapOpencodeStatus(sessionStatus)
		if status != board.AgentNone {
			d.statusCacheMu.Lock()
			d.statusCache[cacheKey] = cachedStatus{
				status:    status,
				timestamp: time.Now(),
			}
			d.statusCacheMu.Unlock()
			return status
		}
	}

	return board.AgentNone
}

func (d *StatusDetector) readStatusFile(sessionName string) board.AgentStatus {
	if sessionName == "" {
		return board.AgentNone
	}

	cacheKey := "file:" + sessionName

	d.statusCacheMu.RLock()
	cached, exists := d.statusCache[cacheKey]
	d.statusCacheMu.RUnlock()

	if exists && time.Since(cached.timestamp) < d.cacheExpiration {
		return cached.status
	}

	var status board.AgentStatus = board.AgentNone

	for _, dir := range d.statusDirs {
		statusFile := filepath.Join(dir, sessionName+".status")
		content, err := os.ReadFile(statusFile)
		if err != nil {
			continue
		}

		statusStr := strings.TrimSpace(string(content))
		switch statusStr {
		case "working":
			status = board.AgentWorking
		case "done", "idle":
			status = board.AgentIdle
		case "waiting", "permission":
			status = board.AgentWaiting
		case "error":
			status = board.AgentError
		case "completed":
			status = board.AgentCompleted
		}

		if status != board.AgentNone {
			break
		}
	}

	d.statusCacheMu.Lock()
	d.statusCache[cacheKey] = cachedStatus{
		status:    status,
		timestamp: time.Now(),
	}
	d.statusCacheMu.Unlock()

	return status
}

func (d *StatusDetector) InvalidateCache(sessionName string) {
	d.statusCacheMu.Lock()
	defer d.statusCacheMu.Unlock()

	if sessionName == "" {
		d.statusCache = make(map[string]cachedStatus)
		return
	}

	delete(d.statusCache, "file:"+sessionName)
	delete(d.statusCache, "opencode:"+sessionName)

	// Port-scoped keys are formatted "opencode-port:<port>:<sessionID>".
	// Clear every entry that belongs to this session, regardless of port.
	suffix := ":" + sessionName
	for key := range d.statusCache {
		if strings.HasPrefix(key, "opencode-port:") && strings.HasSuffix(key, suffix) {
			delete(d.statusCache, key)
		}
	}
}

func WriteStatusFile(sessionName string, status board.AgentStatus) error {
	statusFile := filepath.Join(StatusDir(), sessionName+".status")
	config.GuardHomeWrite(statusFile)

	// Create parent directory for status file (handles slashed session names like "task/my-feature")
	if err := os.MkdirAll(filepath.Dir(statusFile), 0755); err != nil {
		return err
	}
	var statusStr string

	switch status {
	case board.AgentWorking:
		statusStr = "working"
	case board.AgentIdle:
		statusStr = "idle"
	case board.AgentWaiting:
		statusStr = "waiting"
	case board.AgentCompleted:
		statusStr = "completed"
	case board.AgentError:
		statusStr = "error"
	default:
		statusStr = "idle"
	}

	return os.WriteFile(statusFile, []byte(statusStr+"\n"), 0644)
}

func CleanupStatusFile(sessionName string) error {
	statusFile := filepath.Join(StatusDir(), sessionName+".status")
	config.GuardHomeWrite(statusFile)
	os.Remove(statusFile)
	return nil
}

// ReadStatusFile returns the trimmed contents of the session's status
// file. A missing file is not an error — empty string is returned. This
// is an uncached read used by `openkanban status set` to decide whether
// the incoming state would downgrade a terminal "completed" status.
// StatusDetector.readStatusFile is intentionally not reused here: its
// 500ms cache would serve stale values to a guard check made microseconds
// after a write.
func ReadStatusFile(sessionName string) (string, error) {
	statusFile := filepath.Join(StatusDir(), sessionName+".status")
	data, err := os.ReadFile(statusFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ReadAgentStatus reads the on-disk status marker for sessionName and
// returns the corresponding board.AgentStatus, or board.AgentNone when
// no marker exists / the marker can't be parsed. Wraps ReadStatusFile
// for callers (notably the UI startup reconciliation) that want the
// enum rather than the raw string.
func ReadAgentStatus(sessionName string) board.AgentStatus {
	if sessionName == "" {
		return board.AgentNone
	}
	content, err := ReadStatusFile(sessionName)
	if err != nil {
		return board.AgentNone
	}
	switch content {
	case "working":
		return board.AgentWorking
	case "done", "idle":
		return board.AgentIdle
	case "waiting", "permission":
		return board.AgentWaiting
	case "error":
		return board.AgentError
	case "completed":
		return board.AgentCompleted
	}
	return board.AgentNone
}
