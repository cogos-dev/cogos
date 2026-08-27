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

// ─── Rate-limiter defense-in-depth ───────────────────────────────────────────────
//
// The Pong wire type (above) stops a *correctly implemented* peer from
// re-triggering a reply. It does nothing to stop a misbehaving or malicious
// peer from flooding us with genuine Ping frames and forcing unbounded Pong
// output on our side, nor does it protect against a future regression that
// reopens an echo path. pingLimiter bounds outbound Pong replies per
// connection regardless of what triggers them — deliberately not the
// outbound Ping tick, which is self-clocked and can't participate in a
// reflection storm (see the pingTicker.C case in runPeerLoop).

// TestPingLimiterTokenBucket is a non-networked unit test of the token
// bucket itself: burst capacity is consumed immediately, then exhausted
// until enough wall time has elapsed to refill. It is NOT deterministic —
// pingLimiter.allow() reads time.Now() directly with no injectable clock,
// so this test depends on scheduling behaving within the margin below. The
// 150ms interval / 200ms sleep leaves ~100ms of slop before the refill
// count could flip from 1 to 2 (vs. ~40ms with the interval/sleep pair
// tried initially), which held clean across 450 iterations under 12
// concurrent CPU spinners with GOMAXPROCS=1 and -race.
func TestPingLimiterTokenBucket(t *testing.T) {
	l := newPingLimiter(3, 150*time.Millisecond)

	for i := 0; i < 3; i++ {
		if !l.allow() {
			t.Fatalf("token %d of burst 3 should be allowed", i)
		}
	}
	if l.allow() {
		t.Fatal("4th call within the burst window should be denied")
	}

	time.Sleep(200 * time.Millisecond)
	if !l.allow() {
		t.Fatal("after one interval, a refilled token should be allowed")
	}
	if l.allow() {
		t.Fatal("only one token should have refilled after one interval")
	}
}

// TestRunPeerLoopPingFloodIsRateLimited proves the defense-in-depth: a peer
// that floods us with far more genuine Ping frames than our burst allows
// gets only burst-many Pong replies, not one Pong per Ping. Before the rate
// limiter existed, this is exactly the shape a misbehaving peer (or a
// regression reopening the echo) could exploit — every incoming Ping got an
// unconditional reply, no matter how fast they arrived.
func TestRunPeerLoopPingFloodIsRateLimited(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	const burst = 2
	e := &BEPEngine{stopCh: make(chan struct{})}
	pc := &PeerConnection{
		Wire:        bep.NewWire(local),
		closeCh:     make(chan struct{}),
		pingLimiter: newPingLimiter(burst, time.Hour), // long interval: no refill mid-test
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

	const floodSize = 5 // well above burst
	go func() {
		ping := &bep.Ping{}
		for i := 0; i < floodSize; i++ {
			_ = peerWire.WriteMessage(bep.MessageTypePing, ping.Marshal())
		}
	}()

	gotPongs := 0
	for i := 0; i < floodSize; i++ {
		_, _, err := readMessageWithTimeout(t, peerWire, 300*time.Millisecond)
		if err != nil {
			break // limiter kicked in — no more replies coming
		}
		gotPongs++
	}

	if gotPongs != burst {
		t.Fatalf("got %d Pong replies to a flood of %d Pings, want exactly burst=%d — rate limiter did not engage as expected", gotPongs, floodSize, burst)
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
