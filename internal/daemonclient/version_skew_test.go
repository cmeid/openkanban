package daemonclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/techdufus/openkanban/internal/daemon"
)

// TestNew_ProtocolVersionSkew verifies that when the daemon side answers
// HelloResp with a ProtocolVersion that disagrees with the client's
// compiled-in daemon.ProtocolVersion, NewWithConn returns
// ErrProtocolVersionSkew. The test stands up a fake daemon using a
// net.Pipe — no real server, just a goroutine that speaks the codec by
// hand, so the test is independent of the actual server's policy choices.
func TestNew_ProtocolVersionSkew(t *testing.T) {
	clientSide, daemonSide := net.Pipe()

	// Drive the fake daemon: read the HelloReq off the wire, send back
	// HelloResp with ProtocolVersion deliberately bumped by 1 so it
	// won't match daemon.ProtocolVersion no matter what value is
	// compiled in.
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		defer daemonSide.Close()

		r := bufio.NewReader(daemonSide)
		// Expect a JSONReq frame carrying HelloReq.
		typ, payload, err := daemon.ReadFrame(r)
		if err != nil {
			t.Errorf("fake daemon ReadFrame: %v", err)
			return
		}
		if typ != daemon.TypeJSONReq {
			t.Errorf("fake daemon: got frame type 0x%02x want 0x%02x", typ, daemon.TypeJSONReq)
			return
		}
		name, _, err := daemon.DecodeEnvelope(payload)
		if err != nil {
			t.Errorf("fake daemon DecodeEnvelope: %v", err)
			return
		}
		if name != daemon.MsgHelloReq {
			t.Errorf("fake daemon: got msg %q want %q", name, daemon.MsgHelloReq)
			return
		}

		resp := daemon.HelloResp{
			ProtocolVersion: daemon.ProtocolVersion + 1, // deliberate skew
			BinaryVersion:   "fake-daemon",
			ClientCount:     1,
			ClientID:        42,
		}
		raw, err := daemon.EncodeMsg(daemon.MsgHelloResp, resp)
		if err != nil {
			t.Errorf("fake daemon EncodeMsg: %v", err)
			return
		}
		if err := daemon.WriteFrame(daemonSide, daemon.TypeJSONResp, raw); err != nil {
			t.Errorf("fake daemon WriteFrame: %v", err)
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := NewWithConn(ctx, clientSide)
	if err == nil {
		t.Fatalf("NewWithConn: got nil error, want ErrProtocolVersionSkew")
	}
	if !errors.Is(err, ErrProtocolVersionSkew) {
		t.Fatalf("NewWithConn: got %v, want ErrProtocolVersionSkew", err)
	}

	// The error message should carry both versions and the suggested
	// remediation, so a user grepping logs has something to act on.
	msg := err.Error()
	for _, want := range []string{"client=", "daemon=", "openkanban daemon restart"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q: %s", want, msg)
		}
	}

	<-srvDone
}

// TestNew_ProtocolVersionMatch is the happy-path twin of the skew test.
// It confirms NewWithConn does NOT return ErrProtocolVersionSkew when
// the daemon answers with a matching ProtocolVersion. The conn is
// otherwise minimal — the test doesn't exercise any further RPCs.
func TestNew_ProtocolVersionMatch(t *testing.T) {
	clientSide, daemonSide := net.Pipe()

	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		// Keep the conn open so the readLoop doesn't trip a disconnect
		// before the test reads ClientID.
		defer func() {
			// Drain anything the client writes after Hello until the
			// test closes clientSide. We're not asserting on it.
			r := bufio.NewReader(daemonSide)
			for {
				if _, _, err := daemon.ReadFrame(r); err != nil {
					return
				}
			}
		}()

		r := bufio.NewReader(daemonSide)
		typ, payload, err := daemon.ReadFrame(r)
		if err != nil {
			t.Errorf("fake daemon ReadFrame: %v", err)
			return
		}
		if typ != daemon.TypeJSONReq {
			t.Errorf("fake daemon: got frame type 0x%02x want 0x%02x", typ, daemon.TypeJSONReq)
			return
		}
		name, _, err := daemon.DecodeEnvelope(payload)
		if err != nil || name != daemon.MsgHelloReq {
			t.Errorf("fake daemon: name=%q err=%v", name, err)
			return
		}

		resp := daemon.HelloResp{
			ProtocolVersion: daemon.ProtocolVersion, // matching
			BinaryVersion:   "fake-daemon",
			ClientCount:     1,
			ClientID:        7,
		}
		out, err := daemon.EncodeMsg(daemon.MsgHelloResp, resp)
		if err != nil {
			t.Errorf("fake daemon EncodeMsg: %v", err)
			return
		}
		if err := daemon.WriteFrame(daemonSide, daemon.TypeJSONResp, out); err != nil {
			t.Errorf("fake daemon WriteFrame: %v", err)
			return
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c, err := NewWithConn(ctx, clientSide)
	if err != nil {
		t.Fatalf("NewWithConn: %v", err)
	}

	if c.ClientID() != 7 {
		t.Errorf("ClientID: got %d want 7", c.ClientID())
	}
	if c.ProtocolVersion() != daemon.ProtocolVersion {
		t.Errorf("ProtocolVersion: got %d want %d", c.ProtocolVersion(), daemon.ProtocolVersion)
	}

	// Close the client first so the fake-daemon drain loop sees EOF and
	// exits; then wait for srvDone. The reverse order deadlocks.
	_ = c.Close()
	<-srvDone
}
