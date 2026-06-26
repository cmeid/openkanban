package daemon

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestFraming_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{name: "empty json req", typ: TypeJSONReq, payload: []byte{}},
		{name: "one byte pty output", typ: TypePTYOutput, payload: []byte{0xAB}},
		{name: "256 byte pty input", typ: TypePTYInput, payload: bytes.Repeat([]byte{0x42}, 256)},
		{name: "65536 byte json resp", typ: TypeJSONResp, payload: bytes.Repeat([]byte{0x7F}, 65536)},
		{name: "resize sized payload", typ: TypeResize, payload: EncodeResize(80, 24, 7)},
		{name: "detach empty", typ: TypeDetach, payload: nil},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.typ, tc.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}

			gotTyp, gotPayload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if gotTyp != tc.typ {
				t.Errorf("type mismatch: got 0x%02x want 0x%02x", gotTyp, tc.typ)
			}
			// Normalize nil vs empty: payloads are byte-equal modulo length.
			if len(tc.payload) == 0 && len(gotPayload) == 0 {
				return
			}
			if !bytes.Equal(gotPayload, tc.payload) {
				t.Errorf("payload mismatch: got %d bytes want %d bytes", len(gotPayload), len(tc.payload))
			}
		})
	}
}

func TestFraming_TooLarge(t *testing.T) {
	t.Parallel()

	// Direction 1: WriteFrame rejects oversize payloads. The frame total
	// is payload + 1 (type byte), so payload of MaxFrameSize bytes alone
	// trips the limit.
	oversized := make([]byte, MaxFrameSize)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, TypeJSONReq, oversized); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("WriteFrame oversize: got %v want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Errorf("WriteFrame oversize wrote %d bytes; expected 0", buf.Len())
	}

	// Direction 2: ReadFrame rejects a header that declares an oversize
	// length without consuming the (non-existent) payload.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrameSize+1)
	r := bytes.NewReader(hdr[:])
	if _, _, err := ReadFrame(r); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("ReadFrame oversize: got %v want ErrFrameTooLarge", err)
	}
}

func TestFraming_Truncated(t *testing.T) {
	t.Parallel()

	// Header declares a 10-byte frame but we only supply 6 bytes of body.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 10)
	wire := append(hdr[:], bytes.Repeat([]byte{0xCD}, 6)...)

	_, _, err := ReadFrame(bytes.NewReader(wire))
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadFrame truncated body: got %v want io.ErrUnexpectedEOF", err)
	}

	// Also truncate the header itself.
	_, _, err = ReadFrame(bytes.NewReader([]byte{0x00, 0x00})) // only 2 bytes
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("ReadFrame truncated header: got %v want io.ErrUnexpectedEOF", err)
	}
}

func TestFraming_CleanEOF(t *testing.T) {
	t.Parallel()

	_, _, err := ReadFrame(bytes.NewReader(nil))
	if err != io.EOF {
		t.Errorf("ReadFrame empty: got %v want io.EOF", err)
	}
}

func TestResize_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                 string
		cols, rows, clientID uint16
	}{
		{"all zero", 0, 0, 0},
		{"typical", 80, 24, 7},
		{"mid", 1024, 768, 32768},
		{"max", math.MaxUint16, math.MaxUint16, math.MaxUint16},
		{"cols only", 200, 0, 0},
		{"client only", 0, 0, 65535},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := EncodeResize(tc.cols, tc.rows, tc.clientID)
			if len(payload) != 6 {
				t.Fatalf("EncodeResize produced %d bytes, want 6", len(payload))
			}
			cols, rows, clientID, err := DecodeResize(payload)
			if err != nil {
				t.Fatalf("DecodeResize: %v", err)
			}
			if cols != tc.cols || rows != tc.rows || clientID != tc.clientID {
				t.Errorf("round trip mismatch: got (%d,%d,%d) want (%d,%d,%d)",
					cols, rows, clientID, tc.cols, tc.rows, tc.clientID)
			}
		})
	}
}

func TestResize_InvalidLength(t *testing.T) {
	t.Parallel()

	for _, n := range []int{0, 5, 7, 12} {
		if _, _, _, err := DecodeResize(make([]byte, n)); !errors.Is(err, ErrInvalidResize) {
			t.Errorf("DecodeResize len=%d: got %v want ErrInvalidResize", n, err)
		}
	}
}

func TestEnvelope_RoundTrip(t *testing.T) {
	t.Parallel()

	// time.Time round-trips through JSON via RFC3339Nano; we deliberately
	// pick a non-zero fixed instant in UTC so reflect.DeepEqual works.
	startedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	sample := SessionInfo{
		SessionID:      "sess-1",
		TicketID:       "ABC-1",
		SessionName:    "main",
		Workdir:        "/tmp/wd",
		Title:          "Working on X",
		PID:            4242,
		Cols:           80,
		Rows:           24,
		Scrollback:     1000,
		Running:        true,
		AttachedClient: 1,
		StartedAt:      startedAt,
	}

	cases := []struct {
		name    string
		msgType string
		in      any
		out     any
	}{
		{
			"HelloReq",
			MsgHelloReq,
			&HelloReq{ProtocolVersion: ProtocolVersion, BinaryVersion: "0.1.0", ClientName: "openkanban-tui"},
			&HelloReq{},
		},
		{
			"HelloResp",
			MsgHelloResp,
			&HelloResp{ProtocolVersion: ProtocolVersion, BinaryVersion: "0.1.0", ClientCount: 3, ClientID: 7},
			&HelloResp{},
		},
		{
			"SpawnReq",
			MsgSpawnReq,
			&SpawnReq{
				TicketID:    "T-1",
				SessionName: "agent",
				Command:     "claude",
				Workdir:     "/repo",
				Args:        []string{"--foo", "bar"},
				Env:         []string{"PATH=/usr/bin"},
				Cols:        120,
				Rows:        40,
				Scrollback:  10000,
			},
			&SpawnReq{},
		},
		{
			"SpawnResp",
			MsgSpawnResp,
			&SpawnResp{SessionID: "sess-1", PID: 99999},
			&SpawnResp{},
		},
		{
			"KillReq",
			MsgKillReq,
			&KillReq{SessionID: "sess-1", GraceSeconds: 5},
			&KillReq{},
		},
		{
			"KillResp",
			MsgKillResp,
			&KillResp{},
			&KillResp{},
		},
		{
			"TicketDoneReq",
			MsgTicketDoneReq,
			&TicketDoneReq{TicketID: "abc-123"},
			&TicketDoneReq{},
		},
		{
			"TicketDoneResp",
			MsgTicketDoneResp,
			&TicketDoneResp{SessionID: "sess-1", Killed: true},
			&TicketDoneResp{},
		},
		{
			"SessionEventExpected",
			MsgSessionEvent,
			&SessionEvent{Event: "exited", SessionID: "sess-1", TicketID: "T-1", Expected: true, Reason: "ticket_done"},
			&SessionEvent{},
		},
		{
			"ListReq",
			MsgListReq,
			&ListReq{},
			&ListReq{},
		},
		{
			"ListResp",
			MsgListResp,
			&ListResp{Sessions: []SessionInfo{sample}},
			&ListResp{},
		},
		{
			"OwnsReq",
			MsgOwnsReq,
			&OwnsReq{SessionUUID: "11111111-2222-3333-4444-555555555555"},
			&OwnsReq{},
		},
		{
			"OwnsResp",
			MsgOwnsResp,
			&OwnsResp{Owned: true, SessionID: "sess-1"},
			&OwnsResp{},
		},
		{
			"AttachReq",
			MsgAttachReq,
			&AttachReq{SessionID: "sess-1", Cols: 80, Rows: 24, Takeover: true},
			&AttachReq{},
		},
		{
			"AttachResp",
			MsgAttachResp,
			&AttachResp{ClientID: 9, SnapshotSize: 1024},
			&AttachResp{},
		},
		{
			"SubscribeReq",
			MsgSubscribeReq,
			&SubscribeReq{},
			&SubscribeReq{},
		},
		{
			"SubscribeResp",
			MsgSubscribeResp,
			&SubscribeResp{},
			&SubscribeResp{},
		},
		{
			"SessionEvent",
			MsgSessionEvent,
			&SessionEvent{Event: "started", SessionID: "sess-1", TicketID: "T-1", Status: "running"},
			&SessionEvent{},
		},
		{
			"PrepareExitReq",
			MsgPrepareExitReq,
			&PrepareExitReq{},
			&PrepareExitReq{},
		},
		{
			"PrepareExitResp",
			MsgPrepareExitResp,
			&PrepareExitResp{ClientCount: 2, Sessions: []SessionInfo{sample}},
			&PrepareExitResp{},
		},
		{
			"ShutdownReq",
			MsgShutdownReq,
			&ShutdownReq{Force: true},
			&ShutdownReq{},
		},
		{
			"ShutdownResp",
			MsgShutdownResp,
			&ShutdownResp{KilledSessions: 3},
			&ShutdownResp{},
		},
		{
			"ErrorResp",
			MsgErrorResp,
			&ErrorResp{Code: "not_found", Message: "session does not exist"},
			&ErrorResp{},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw, err := EncodeMsg(tc.msgType, tc.in)
			if err != nil {
				t.Fatalf("EncodeMsg: %v", err)
			}

			gotType, payload, err := DecodeEnvelope(raw)
			if err != nil {
				t.Fatalf("DecodeEnvelope: %v", err)
			}
			if gotType != tc.msgType {
				t.Errorf("type name: got %q want %q", gotType, tc.msgType)
			}

			if err := json.Unmarshal(payload, tc.out); err != nil {
				t.Fatalf("unmarshal payload into %T: %v", tc.out, err)
			}

			if !reflect.DeepEqual(tc.in, tc.out) {
				t.Errorf("round trip mismatch:\n  in:  %#v\n  out: %#v", tc.in, tc.out)
			}
		})
	}
}

func TestEnvelope_UnknownType(t *testing.T) {
	t.Parallel()

	// An envelope with a type the caller doesn't recognize must still
	// decode cleanly; it's the caller's job to switch on the name.
	raw, err := EncodeMsg("future.unknown.req", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("EncodeMsg: %v", err)
	}

	typeName, payload, err := DecodeEnvelope(raw)
	if err != nil {
		t.Errorf("DecodeEnvelope unknown type: unexpected error %v", err)
	}
	if typeName != "future.unknown.req" {
		t.Errorf("type name: got %q want %q", typeName, "future.unknown.req")
	}
	if len(payload) == 0 {
		t.Errorf("payload missing for unknown type envelope")
	}
}
