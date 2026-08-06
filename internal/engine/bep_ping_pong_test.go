// bep_ping_pong_test.go — regression guard for the Ping/Pong volley bug.
//
// runPeerLoop used to reply to an incoming Ping with ANOTHER Ping (there was
// no distinct Pong type). Two symmetric peers would therefore volley Ping
// frames forever once either side's pingTicker fired once, fully decoupled
// from the 90s ticker cadence — this was confirmed live between two peers at
// ~985 KB/s bidirectional. The fix adds bep.MessageTypePong: Ping still
// provokes exactly one Pong reply, but a received Pong must NEVER itself
// provoke a reply. That second half is what this test proves.

package engine

import (
	"net"
	"testing"
	"time"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

func TestRunPeerLoopPingGetsPongReply(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	e := &BEPEngine{stopCh: make(chan struct{})}
	pc := &PeerConnection{
		Wire:    bep.NewWire(local),
		closeCh: make(chan struct{}),
	}

	loopDone := make(chan struct{})
	go func() {
		e.runPeerLoop(pc, bep.DeviceID{})
		close(loopDone)
	}()
	defer func() {
		close(pc.closeCh)
		remote.Close()
		<-loopDone
	}()

	peerWire := bep.NewWire(remote)

	ping := &bep.Ping{}
	if err := peerWire.WriteMessage(bep.MessageTypePing, ping.Marshal()); err != nil {
		t.Fatalf("write ping: %v", err)
	}

	msgType, _, err := readMessageWithTimeout(t, peerWire, 2*time.Second)
	if err != nil {
		t.Fatalf("expected a Pong reply to Ping, got error: %v", err)
	}
	if msgType != bep.MessageTypePong {
		t.Fatalf("reply to Ping = message type %d, want MessageTypePong (%d)", msgType, bep.MessageTypePong)
	}
}

// TestRunPeerLoopPongDoesNotReply is the regression guard: a received Pong
// must not itself produce any outbound message. Before the fix, this path
// didn't exist as a distinct case at all — Ping was answered with Ping,
// which is exactly the shape that makes two symmetric peers volley forever.
func TestRunPeerLoopPongDoesNotReply(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	e := &BEPEngine{stopCh: make(chan struct{})}
	pc := &PeerConnection{
		Wire:    bep.NewWire(local),
		closeCh: make(chan struct{}),
	}

	loopDone := make(chan struct{})
	go func() {
		e.runPeerLoop(pc, bep.DeviceID{})
		close(loopDone)
	}()
	defer func() {
		close(pc.closeCh)
		remote.Close()
		<-loopDone
	}()

	peerWire := bep.NewWire(remote)

	pong := &bep.Pong{}
	if err := peerWire.WriteMessage(bep.MessageTypePong, pong.Marshal()); err != nil {
		t.Fatalf("write pong: %v", err)
	}

	msgType, _, err := readMessageWithTimeout(t, peerWire, 300*time.Millisecond)
	if err == nil {
		t.Fatalf("expected no reply to Pong, but got message type %d", msgType)
	}
}

// readMessageWithTimeout reads one message from w, returning a timeout error
// if nothing arrives within d. net.Pipe conns don't buffer, so a bounded
// read is the only way to assert "nothing was sent."
func readMessageWithTimeout(t *testing.T, w *bep.Wire, d time.Duration) (bep.MessageType, []byte, error) {
	t.Helper()

	type result struct {
		msgType bep.MessageType
		payload []byte
		err     error
	}
	resCh := make(chan result, 1)
	go func() {
		msgType, payload, err := w.ReadMessage()
		resCh <- result{msgType, payload, err}
	}()

	select {
	case res := <-resCh:
		return res.msgType, res.payload, res.err
	case <-time.After(d):
		return 0, nil, errTimeout
	}
}

var errTimeout = &timeoutError{}

type timeoutError struct{}

func (*timeoutError) Error() string { return "timed out waiting for message" }
