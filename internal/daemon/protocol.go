// Package daemon defines the wire protocol used between openkanban TUI
// clients and the openkanbankd daemon over a per-user Unix socket.
//
// Wire format
//
// Every message on the connection is a length-prefixed frame:
//
//	[uint32 big-endian length][1 byte type][payload ... length-1 bytes]
//
// The length header counts the type byte plus the payload. The maximum
// allowed frame size (including the type byte) is MaxFrameSize (1 MiB);
// larger frames are rejected on both encode and decode.
//
// Connection mode
//
// A connection starts in JSON mode. JSON-mode frames carry one of:
//
//	TypeJSONReq  - client -> daemon request
//	TypeJSONResp - daemon -> client response to a request
//	TypeJSONPush - daemon -> client push (subscription event)
//
// Their payload is a JSON-encoded Envelope tagging a concrete message
// type by its MsgXxx constant.
//
// After a successful Attach request the connection upgrades to binary
// mode for that PTY. In binary mode the frames carry raw PTY bytes and
// control signals:
//
//	TypePTYOutput - daemon -> client, raw bytes from the PTY
//	TypePTYInput  - client -> daemon, raw bytes for stdin
//	TypeResize    - either direction, a 6-byte encoded resize payload
//	TypeDetach    - either direction, signals the binary session is done
//
// The package is pure encode/decode: it does no I/O of its own beyond
// the io.Reader / io.Writer arguments to WriteFrame and ReadFrame, and
// has no goroutines, channels, or networking.
package daemon

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// ProtocolVersion is the current wire-protocol version. Clients and the
// daemon exchange this in Hello to confirm compatibility.
const ProtocolVersion uint16 = 1

// MaxFrameSize is the maximum allowed total frame size (including the
// type byte). Frames larger than this are rejected.
const MaxFrameSize uint32 = 1 << 20

// Frame type bytes. Values in 0x00-0x0F are reserved for JSON-mode
// control traffic; 0x10-0x1F are reserved for binary PTY traffic.
const (
	TypeJSONReq   byte = 0x01
	TypeJSONResp  byte = 0x02
	TypeJSONPush  byte = 0x03
	TypePTYOutput byte = 0x10
	TypePTYInput  byte = 0x11
	TypeResize    byte = 0x12
	TypeDetach    byte = 0x13
)

// ErrFrameTooLarge is returned when a frame's declared or actual size
// exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("daemon: frame exceeds MaxFrameSize")

// ErrInvalidResize is returned when a resize payload is not exactly
// 6 bytes long.
var ErrInvalidResize = errors.New("daemon: invalid resize payload")

// WriteFrame writes a single framed message to w. The on-wire length
// header includes the type byte, so the total bytes written are
// 4 + 1 + len(payload). WriteFrame returns ErrFrameTooLarge if the
// resulting frame would exceed MaxFrameSize.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	total := uint64(len(payload)) + 1 // +1 for type byte
	if total > uint64(MaxFrameSize) {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(total))
	hdr[4] = typ
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// ReadFrame reads a single framed message from r. It returns the type
// byte and the payload (which may be empty). On clean disconnect at a
// frame boundary it returns io.EOF unchanged; a partial frame returns
// io.ErrUnexpectedEOF. A frame whose declared length exceeds
// MaxFrameSize returns ErrFrameTooLarge without consuming the payload.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [4]byte
	n, err := io.ReadFull(r, hdr[:])
	if err != nil {
		// io.ReadFull returns io.EOF when zero bytes were read (clean
		// disconnect) and io.ErrUnexpectedEOF on a partial read. Pass
		// both through unchanged so callers can distinguish them.
		if err == io.EOF && n == 0 {
			return 0, nil, io.EOF
		}
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(hdr[:])
	if length == 0 {
		// A length of 0 means no type byte at all, which is malformed.
		return 0, nil, fmt.Errorf("daemon: zero-length frame")
	}
	if length > MaxFrameSize {
		return 0, nil, ErrFrameTooLarge
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return body[0], body[1:], nil
}

// EncodeResize encodes a resize event as 6 big-endian bytes:
// cols, rows, clientID (each uint16).
func EncodeResize(cols, rows, clientID uint16) []byte {
	out := make([]byte, 6)
	binary.BigEndian.PutUint16(out[0:2], cols)
	binary.BigEndian.PutUint16(out[2:4], rows)
	binary.BigEndian.PutUint16(out[4:6], clientID)
	return out
}

// DecodeResize parses a 6-byte resize payload. It returns
// ErrInvalidResize if the payload is not exactly 6 bytes long.
func DecodeResize(payload []byte) (cols, rows, clientID uint16, err error) {
	if len(payload) != 6 {
		return 0, 0, 0, ErrInvalidResize
	}
	cols = binary.BigEndian.Uint16(payload[0:2])
	rows = binary.BigEndian.Uint16(payload[2:4])
	clientID = binary.BigEndian.Uint16(payload[4:6])
	return cols, rows, clientID, nil
}

// Envelope is the JSON-mode wrapper that tags a payload with its
// concrete message type. The Type field always carries one of the
// MsgXxx constants below; Payload is the JSON-encoded body.
type Envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Message-type constants. The .req / .resp / .event suffixes are used
// consistently so a single Envelope can route any direction of traffic.
const (
	MsgHelloReq  = "hello.req"
	MsgHelloResp = "hello.resp"

	MsgSpawnReq  = "spawn.req"
	MsgSpawnResp = "spawn.resp"
	MsgKillReq   = "kill.req"
	MsgKillResp  = "kill.resp"

	MsgListReq  = "list.req"
	MsgListResp = "list.resp"
	MsgOwnsReq  = "owns.req"
	MsgOwnsResp = "owns.resp"

	MsgAttachReq  = "attach.req"
	MsgAttachResp = "attach.resp"

	MsgSubscribeReq  = "subscribe.req"
	MsgSubscribeResp = "subscribe.resp"
	MsgSessionEvent  = "session.event"

	MsgPrepareExitReq  = "prepare_exit.req"
	MsgPrepareExitResp = "prepare_exit.resp"
	MsgShutdownReq     = "shutdown.req"
	MsgShutdownResp    = "shutdown.resp"

	MsgErrorResp = "error.resp"
)

// EncodeMsg marshals payload into an Envelope tagged with typeName and
// returns the resulting JSON bytes.
func EncodeMsg(typeName string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("daemon: marshal payload: %w", err)
	}
	env := Envelope{Type: typeName, Payload: raw}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("daemon: marshal envelope: %w", err)
	}
	return out, nil
}

// DecodeEnvelope extracts the type name and raw payload from an
// envelope-shaped JSON blob without decoding the payload. The caller is
// expected to switch on typeName and unmarshal payload into the
// matching concrete struct.
func DecodeEnvelope(data []byte) (typeName string, payload json.RawMessage, err error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", nil, fmt.Errorf("daemon: unmarshal envelope: %w", err)
	}
	return env.Type, env.Payload, nil
}

// HelloReq is the first message a client sends after connecting. It
// announces the client's protocol version and identity.
type HelloReq struct {
	ProtocolVersion uint16 `json:"protocol_version"`
	BinaryVersion   string `json:"binary_version"`
	ClientName      string `json:"client_name"`
}

// HelloResp is the daemon's response to HelloReq. ClientID uniquely
// identifies this connection within the daemon for the lifetime of the
// connection.
type HelloResp struct {
	ProtocolVersion uint16 `json:"protocol_version"`
	BinaryVersion   string `json:"binary_version"`
	ClientCount     int    `json:"client_count"`
	ClientID        uint16 `json:"client_id"`
}

// SpawnReq asks the daemon to start a new PTY-backed session.
//
// AgentSessionUUID is the Claude / opencode session UUID this spawn is
// linked to (i.e. the value that ended up in ticket.AgentSessionID, if
// set). It is recorded on the daemon-side Session so that Owns queries
// can answer "do you own session <UUID>?" without rummaging through the
// pane / agent process internals. It is purely informational on the
// daemon side — the spawn command line still drives whether the agent
// actually resumes that UUID.
type SpawnReq struct {
	TicketID         string   `json:"ticket_id"`
	SessionName      string   `json:"session_name"`
	Command          string   `json:"command"`
	Workdir          string   `json:"workdir"`
	Args             []string `json:"args,omitempty"`
	Env              []string `json:"env,omitempty"`
	Cols             int      `json:"cols"`
	Rows             int      `json:"rows"`
	Scrollback       int      `json:"scrollback"`
	AgentSessionUUID string   `json:"agent_session_uuid,omitempty"`
}

// SpawnResp acknowledges a successful spawn.
type SpawnResp struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
}

// KillReq asks the daemon to terminate a session. GraceSeconds is the
// SIGTERM-to-SIGKILL grace window; the field uses int rather than
// time.Duration to make JSON wire encoding explicit.
type KillReq struct {
	SessionID    string `json:"session_id"`
	GraceSeconds int    `json:"grace_seconds"`
}

// KillResp acknowledges a successful kill.
type KillResp struct{}

// ListReq asks for the current set of daemon-owned sessions.
type ListReq struct{}

// ListResp returns all live sessions known to the daemon.
type ListResp struct {
	Sessions []SessionInfo `json:"sessions"`
}

// SessionInfo describes a single daemon-owned session.
type SessionInfo struct {
	SessionID      string    `json:"session_id"`
	TicketID       string    `json:"ticket_id"`
	SessionName    string    `json:"session_name"`
	Workdir        string    `json:"workdir"`
	Title          string    `json:"title"`
	PID            int       `json:"pid"`
	Cols           int       `json:"cols"`
	Rows           int       `json:"rows"`
	Running        bool      `json:"running"`
	AttachedClient uint16    `json:"attached_client"`
	StartedAt      time.Time `json:"started_at"`
}

// OwnsReq asks whether the daemon currently owns the agent session
// identified by SessionUUID (the Claude / opencode session UUID, not
// the daemon-internal SessionID).
type OwnsReq struct {
	SessionUUID string `json:"session_uuid"`
}

// OwnsResp answers an OwnsReq. If Owned is true, SessionID is the
// daemon-internal handle the client may Attach to.
type OwnsResp struct {
	Owned     bool   `json:"owned"`
	SessionID string `json:"session_id"`
}

// AttachReq requests that the connection upgrade to binary mode and
// stream the named session's PTY. If Takeover is true the daemon will
// detach any existing client first.
type AttachReq struct {
	SessionID string `json:"session_id"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
	Takeover  bool   `json:"takeover"`
}

// AttachResp acknowledges a successful Attach. The daemon immediately
// follows the AttachResp frame with SnapshotSize bytes' worth of
// TypePTYOutput frames (the scrollback snapshot) before the connection
// is fully in binary mode.
type AttachResp struct {
	ClientID     uint16 `json:"client_id"`
	SnapshotSize int    `json:"snapshot_size"`
}

// SubscribeReq asks the daemon to start pushing SessionEvent messages
// over this connection.
type SubscribeReq struct{}

// SubscribeResp acknowledges the subscription.
type SubscribeResp struct{}

// SessionEvent is a server-push update about a session. Event is one
// of "started", "exited", "attached", "detached", "status".
type SessionEvent struct {
	Event     string `json:"event"`
	SessionID string `json:"session_id"`
	TicketID  string `json:"ticket_id"`
	Status    string `json:"status"`
}

// PrepareExitReq is sent by a TUI client that is about to quit; it
// allows the daemon to report how many other clients remain and what
// sessions are still owned, so the client can warn the user.
type PrepareExitReq struct{}

// PrepareExitResp tells the exiting client what state is at stake.
type PrepareExitResp struct {
	ClientCount int           `json:"client_count"`
	Sessions    []SessionInfo `json:"sessions"`
}

// ShutdownReq asks the daemon to exit. With Force=false the daemon
// refuses if any sessions are still running.
type ShutdownReq struct {
	Force bool `json:"force"`
}

// ShutdownResp reports how many sessions the shutdown killed (only
// nonzero when Force was true).
type ShutdownResp struct {
	KilledSessions int `json:"killed_sessions"`
}

// ErrorResp is the generic error response. Code is a stable
// machine-readable identifier; Message is a human-readable explanation.
type ErrorResp struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
