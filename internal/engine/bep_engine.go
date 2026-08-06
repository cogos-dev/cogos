// bep_engine.go — BEPEngine: TLS listener, peer dialer, connection management,
// and BEP protocol lifecycle. Extracted from root package main into internal/engine
// as Phase 2 S1 of the cross-node transport work (see issue #330).
//
// Integration pattern:
//
//	Create → SetEventCallback (optional) → Start() → defer Stop()
//
// Per-connection: TLS handshake → Hello → ClusterConfig → Index → steady-state
// message loop with 90s Ping interval.
//
// The busSessionManager coupling has been inverted: callers supply a
// func(bep.SyncEvent) callback via SetEventCallback instead of receiving a
// pointer to the bus internals.

package engine

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sync"
	"time"

	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── BEPEngine ──────────────────────────────────────────────────────────────────

// BEPEngine manages BEP transport for cross-node agent CRD sync.
type BEPEngine struct {
	mu sync.Mutex

	root      string
	config    *bep.Config
	deviceID  bep.DeviceID
	shortID   uint64
	tlsConfig *tls.Config
	tlsCert   tls.Certificate
	listener  net.Listener

	provider *BEPProvider
	model    *AgentSyncModel
	onEvent  func(bep.SyncEvent) // callback replacing busSessionManager coupling

	peers   map[bep.DeviceID]*PeerConnection
	peersMu sync.RWMutex

	stopCh  chan struct{}
	stopped bool
	wg      sync.WaitGroup

	// Phase 2 S4: remote dispatch over BEP (see bep_dispatch.go).
	dispatchFields
}

// PeerConnection represents an active connection to a peer node.
type PeerConnection struct {
	DeviceID  bep.DeviceID
	Name      string
	Address   string
	Wire      *bep.Wire
	Connected bool
	LastPing  time.Time
	LastPong  time.Time
	closeCh   chan struct{}
	closeOnce sync.Once
}

// ─── Constructor ────────────────────────────────────────────────────────────────

// NewBEPEngine creates a new engine from cluster config.
// Does not start listening or connecting — call Start() for that.
func NewBEPEngine(root string, config *bep.Config, provider *BEPProvider) (*BEPEngine, error) {
	// Resolve cert directory.
	certDir := bep.ExpandCertDir(config.CertDir)

	// Load TLS certificate.
	cert, err := bep.LoadBEPCert(certDir)
	if err != nil {
		return nil, fmt.Errorf("load BEP cert: %w", err)
	}

	// Derive DeviceID.
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}
	deviceID := bep.DeviceIDFromCert(x509Cert)
	shortID := bep.ShortIDFromDeviceID(deviceID)

	// Build trusted peers set.
	trustedPeers := make(map[bep.DeviceID]bool)
	for _, p := range config.Peers {
		if p.Trusted {
			pid, err := bep.ParseDeviceID(p.DeviceID)
			if err != nil {
				log.Printf("[bep-engine] invalid peer device ID %q: %v", p.DeviceID, err)
				continue
			}
			trustedPeers[pid] = true
		}
	}

	tlsConfig := bep.TLSConfig(cert, func(id bep.DeviceID) bool {
		return trustedPeers[id]
	})

	engine := &BEPEngine{
		root:      root,
		config:    config,
		deviceID:  deviceID,
		shortID:   shortID,
		tlsConfig: tlsConfig,
		tlsCert:   cert,
		provider:  provider,
		peers:     make(map[bep.DeviceID]*PeerConnection),
		stopCh:    make(chan struct{}),
		dispatchFields: dispatchFields{
			inflight: make(map[uint32]*dispatchInFlight),
		},
	}

	// Create sync model.
	stateDir := filepath.Join(root, ".cog", ".state", "bep")
	engine.model = NewAgentSyncModel(engine, provider.watchDir, stateDir, shortID)

	return engine, nil
}

// ─── Lifecycle ──────────────────────────────────────────────────────────────────

// SetEventCallback registers a callback that receives SyncEvents as they occur.
// This replaces the previous SetBus(*busSessionManager) coupling — callers
// supply a plain func and handle bus forwarding in their own layer.
// Safe to call before or after Start(); replaces any previously registered callback.
func (e *BEPEngine) SetEventCallback(fn func(bep.SyncEvent)) {
	e.mu.Lock()
	e.onEvent = fn
	e.mu.Unlock()
	e.model.SetEventEmitter(func(evt bep.SyncEvent) {
		evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
		bep.EmitSyncEvent(evt.Type, evt.Summary)
		e.mu.Lock()
		cb := e.onEvent
		e.mu.Unlock()
		if cb != nil {
			cb(evt)
		}
	})
}

// Start begins listening for incoming connections and dials configured peers.
func (e *BEPEngine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// ListenPort 0 means OS-assigned random port (useful for tests).
	// Enabled nodes loaded from cluster.yaml are defaulted to
	// bep.DefaultListenPort by BEPProvider.LoadConfig when unset (#336).
	addr := fmt.Sprintf(":%d", e.config.ListenPort)
	ln, err := tls.Listen("tcp", addr, e.tlsConfig)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	e.listener = ln

	log.Printf("[bep-engine] started, device=%s listen=%s peers=%d",
		bep.FormatDeviceID(e.deviceID)[:7], ln.Addr(), len(e.config.Peers))

	bep.EmitEngineStarted(bep.FormatDeviceID(e.deviceID), ln.Addr().String(), len(e.config.Peers))

	// Accept incoming connections.
	e.wg.Add(1)
	go e.runListener()

	// Dial configured peers.
	for _, peer := range e.config.Peers {
		if !peer.Trusted || peer.Address == "" {
			continue
		}
		p := peer // capture
		e.wg.Add(1)
		go e.runDialer(p)
	}

	return nil
}

// Stop shuts down the engine: closes listener, disconnects peers.
func (e *BEPEngine) Stop() {
	e.mu.Lock()
	if e.stopped {
		e.mu.Unlock()
		return
	}
	e.stopped = true
	close(e.stopCh)
	e.mu.Unlock()

	if e.listener != nil {
		e.listener.Close()
	}

	// Close all peer connections.
	e.peersMu.Lock()
	for _, pc := range e.peers {
		pc.Close()
	}
	e.peersMu.Unlock()

	e.wg.Wait()
	bep.EmitEngineStopped("shutdown")
	log.Printf("[bep-engine] stopped")
}

// ─── Listener ───────────────────────────────────────────────────────────────────

func (e *BEPEngine) runListener() {
	defer e.wg.Done()

	for {
		conn, err := e.listener.Accept()
		if err != nil {
			select {
			case <-e.stopCh:
				return
			default:
				log.Printf("[bep-engine] accept error: %v", err)
				continue
			}
		}

		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.handleConnection(conn, true)
		}()
	}
}

// ─── Dialer ─────────────────────────────────────────────────────────────────────

func (e *BEPEngine) runDialer(peer bep.Peer) {
	defer e.wg.Done()

	backoff := time.Second
	maxBackoff := 5 * time.Minute

	for {
		select {
		case <-e.stopCh:
			return
		default:
		}

		// Check if already connected.
		peerID, err := bep.ParseDeviceID(peer.DeviceID)
		if err != nil {
			log.Printf("[bep-engine] invalid peer ID %q: %v", peer.DeviceID, err)
			return
		}

		e.peersMu.RLock()
		_, alreadyConnected := e.peers[peerID]
		e.peersMu.RUnlock()
		if alreadyConnected {
			// Wait and re-check.
			select {
			case <-e.stopCh:
				return
			case <-time.After(30 * time.Second):
				continue
			}
		}

		// Dial with timeout.
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", peer.Address, e.tlsConfig)
		if err != nil {
			log.Printf("[bep-engine] dial %s (%s): %v", peer.Name, peer.Address, err)
			select {
			case <-e.stopCh:
				return
			case <-time.After(backoff):
				backoff = backoff * 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
		}

		// Reset backoff on successful connection.
		backoff = time.Second
		e.handleConnection(conn, false)
	}
}

// ─── Connection handler ─────────────────────────────────────────────────────────

func (e *BEPEngine) handleConnection(conn net.Conn, inbound bool) {
	wire := bep.NewWire(conn)
	defer wire.Close()

	// Extract peer DeviceID from TLS state.
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		log.Printf("[bep-engine] non-TLS connection rejected")
		return
	}

	// Complete TLS handshake.
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("[bep-engine] TLS handshake failed: %v", err)
		return
	}

	state := tlsConn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		log.Printf("[bep-engine] no peer certificate")
		return
	}
	peerID := bep.DeviceIDFromCert(state.PeerCertificates[0])
	peerShort := bep.FormatDeviceID(peerID)[:7]

	direction := "inbound"
	if !inbound {
		direction = "outbound"
	}
	log.Printf("[bep-engine] %s connection from %s (%s)", direction, peerShort, conn.RemoteAddr())

	// 1. Hello exchange.
	deviceName := bep.FormatDeviceID(e.deviceID)[:7]
	if e.config.NodeName != "" {
		deviceName = e.config.NodeName
	}
	hello := &bep.Hello{
		DeviceName:    deviceName,
		ClientName:    "cogos",
		ClientVersion: Version,
	}
	if err := wire.WriteHello(hello); err != nil {
		log.Printf("[bep-engine] failed to send hello to %s: %v", peerShort, err)
		return
	}

	peerHello, err := wire.ReadHello()
	if err != nil {
		log.Printf("[bep-engine] failed to read hello from %s: %v", peerShort, err)
		return
	}
	log.Printf("[bep-engine] hello from %s: %s/%s", peerShort, peerHello.ClientName, peerHello.ClientVersion)

	// 2. ClusterConfig exchange.
	cc := &bep.ClusterConfig{
		Folders: []*bep.Folder{{
			ID:    agentSyncFolderID,
			Label: "Agent Definitions",
			Devices: []*bep.Device{
				{ID: e.deviceID[:], Name: hello.DeviceName},
				{ID: peerID[:], Name: peerHello.DeviceName},
			},
		}},
	}
	if err := wire.WriteMessage(bep.MessageTypeClusterConfig, cc.Marshal()); err != nil {
		log.Printf("[bep-engine] failed to send cluster config to %s: %v", peerShort, err)
		return
	}

	_, ccPayload, err := wire.ReadMessage()
	if err != nil {
		log.Printf("[bep-engine] failed to read cluster config from %s: %v", peerShort, err)
		return
	}
	peerCC := &bep.ClusterConfig{}
	if err := peerCC.Unmarshal(ccPayload); err != nil {
		log.Printf("[bep-engine] failed to parse cluster config from %s: %v", peerShort, err)
		return
	}

	// 3. Register peer connection.
	pc := &PeerConnection{
		DeviceID:  peerID,
		Name:      peerHello.DeviceName,
		Address:   conn.RemoteAddr().String(),
		Wire:      wire,
		Connected: true,
		LastPing:  time.Now(),
		closeCh:   make(chan struct{}),
	}

	e.peersMu.Lock()
	if existing, ok := e.peers[peerID]; ok {
		existing.Close()
	}
	e.peers[peerID] = pc
	e.peersMu.Unlock()

	bep.EmitPeerConnected(peerShort, peerHello.DeviceName)

	defer func() {
		e.peersMu.Lock()
		if e.peers[peerID] == pc {
			delete(e.peers, peerID)
		}
		e.peersMu.Unlock()
		bep.EmitPeerDisconnected(peerShort, "connection closed")
	}()

	// 4. Send full Index.
	files := e.model.LoadAndScanIndex()
	idx := &bep.Index{
		Folder: agentSyncFolderID,
		Files:  files,
	}
	if err := wire.WriteMessage(bep.MessageTypeIndex, idx.Marshal()); err != nil {
		log.Printf("[bep-engine] failed to send index to %s: %v", peerShort, err)
		return
	}

	// 5. Steady-state message loop.
	e.runPeerLoop(pc, peerID)
}

// ─── Peer message loop ──────────────────────────────────────────────────────────

func (e *BEPEngine) runPeerLoop(pc *PeerConnection, peerID bep.DeviceID) {
	peerShort := bep.FormatDeviceID(peerID)[:7]

	// Ping ticker: send Ping every 90 seconds.
	pingTicker := time.NewTicker(90 * time.Second)
	defer pingTicker.Stop()

	// Read messages in a goroutine.
	msgCh := make(chan struct {
		msgType bep.MessageType
		payload []byte
		err     error
	}, 1)

	go func() {
		for {
			msgType, payload, err := pc.Wire.ReadMessage()
			select {
			case msgCh <- struct {
				msgType bep.MessageType
				payload []byte
				err     error
			}{msgType, payload, err}:
			case <-pc.closeCh:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-e.stopCh:
			return

		case <-pc.closeCh:
			return

		case <-pingTicker.C:
			ping := &bep.Ping{}
			if err := pc.Wire.WriteMessage(bep.MessageTypePing, ping.Marshal()); err != nil {
				log.Printf("[bep-engine] ping to %s failed: %v", peerShort, err)
				return
			}
			pc.LastPing = time.Now()

		case msg := <-msgCh:
			if msg.err != nil {
				log.Printf("[bep-engine] read from %s failed: %v", peerShort, msg.err)
				return
			}

			switch msg.msgType {
			case bep.MessageTypeIndex:
				idx := &bep.Index{}
				if err := idx.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad index from %s: %v", peerShort, err)
					continue
				}
				e.model.HandleIndex(peerID, idx.Files)

			case bep.MessageTypeIndexUpdate:
				update := &bep.Index{}
				if err := update.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad index update from %s: %v", peerShort, err)
					continue
				}
				e.model.HandleIndexUpdate(peerID, update.Files)

			case bep.MessageTypeRequest:
				req := &bep.Request{}
				if err := req.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad request from %s: %v", peerShort, err)
					continue
				}
				resp := e.model.HandleRequest(req)
				if err := pc.Wire.WriteMessage(bep.MessageTypeResponse, resp.Marshal()); err != nil {
					log.Printf("[bep-engine] failed to send response to %s: %v", peerShort, err)
					return
				}

			case bep.MessageTypeResponse:
				resp := &bep.Response{}
				if err := resp.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad response from %s: %v", peerShort, err)
					continue
				}
				e.model.HandleResponse(resp)

			case bep.MessageTypePing:
				// Reply with Pong. Pong must never itself trigger a reply,
				// or two peers volley Ping frames forever (see
				// MessageTypePong docs in pkg/substrate/bep/proto.go).
				pong := &bep.Pong{}
				if err := pc.Wire.WriteMessage(bep.MessageTypePong, pong.Marshal()); err != nil {
					log.Printf("[bep-engine] pong to %s failed: %v", peerShort, err)
					return
				}

			case bep.MessageTypePong:
				// Liveness only — record and do NOT reply. Replying here is
				// the whole bug: it turns one Ping into an infinite volley
				// between two symmetric peers.
				pc.LastPong = time.Now()

			case bep.MessageTypeDispatch:
				// Phase 2 S4: incoming remote dispatch request — run locally
				// and send the result back to the peer. Runs in a goroutine so
				// the peer loop is never blocked while a dispatch executes.
				capturedPc := pc
				capturedPayload := msg.payload
				e.wg.Add(1)
				go func() {
					defer e.wg.Done()
					e.handleDispatchMessage(capturedPc, capturedPayload)
				}()

			case bep.MessageTypeDispatchResult:
				// Phase 2 S4: incoming result for a locally-initiated remote
				// dispatch — deliver to the waiting RemoteDispatch caller.
				e.handleDispatchResultMessage(msg.payload)

			case bep.MessageTypeClose:
				cl := &bep.Close{}
				if err := cl.Unmarshal(msg.payload); err == nil {
					log.Printf("[bep-engine] peer %s closed: %s", peerShort, cl.Reason)
				}
				return

			default:
				log.Printf("[bep-engine] unknown message type %d from %s", msg.msgType, peerShort)
			}
		}
	}
}

// ─── Message sending ────────────────────────────────────────────────────────────

// SendToPeer sends a message to a specific peer.
func (e *BEPEngine) SendToPeer(peerID bep.DeviceID, msgType bep.MessageType, payload []byte) {
	e.peersMu.RLock()
	pc, ok := e.peers[peerID]
	e.peersMu.RUnlock()
	if !ok {
		return
	}
	if err := pc.Wire.WriteMessage(msgType, payload); err != nil {
		log.Printf("[bep-engine] send to %s failed: %v", bep.FormatDeviceID(peerID)[:7], err)
	}
}

// BroadcastMessage sends a message to all connected peers.
func (e *BEPEngine) BroadcastMessage(msgType bep.MessageType, payload []byte) {
	e.peersMu.RLock()
	peers := make([]*PeerConnection, 0, len(e.peers))
	for _, pc := range e.peers {
		peers = append(peers, pc)
	}
	e.peersMu.RUnlock()

	for _, pc := range peers {
		if err := pc.Wire.WriteMessage(msgType, payload); err != nil {
			log.Printf("[bep-engine] broadcast to %s failed: %v",
				bep.FormatDeviceID(pc.DeviceID)[:7], err)
		}
	}
}

// NotifyLocalChange is called by BEPProvider when a local CRD file changes.
func (e *BEPEngine) NotifyLocalChange(filename string) {
	e.model.NotifyLocalChange(filename)
}

// ─── PeerConnection methods ─────────────────────────────────────────────────────

// Close terminates a peer connection.
func (pc *PeerConnection) Close() {
	pc.closeOnce.Do(func() {
		close(pc.closeCh)
		pc.Connected = false
		if pc.Wire != nil {
			// Send Close message (best effort).
			cl := &bep.Close{Reason: "shutdown"}
			_ = pc.Wire.WriteMessage(bep.MessageTypeClose, cl.Marshal())
			pc.Wire.Close()
		}
	})
}

// ─── Status ─────────────────────────────────────────────────────────────────────

// ListenerAddr returns the engine's listen address, or "" if not started.
// Provided for test access without exposing the internal net.Listener.
func (e *BEPEngine) ListenerAddr() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.listener == nil {
		return ""
	}
	return e.listener.Addr().String()
}

// ConnectedPeerCount returns the number of currently connected peers.
// Provided for test access without exposing the internal peers map.
func (e *BEPEngine) ConnectedPeerCount() int {
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()
	return len(e.peers)
}

// Status returns the current engine status.
func (e *BEPEngine) Status() bep.EngineStatus {
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()

	status := bep.EngineStatus{
		Running:  true,
		DeviceID: bep.FormatDeviceID(e.deviceID),
	}
	if e.listener != nil {
		status.ListenAddr = e.listener.Addr().String()
	}
	status.PeerCount = len(e.peers)

	for _, pc := range e.peers {
		status.Peers = append(status.Peers, bep.PeerStatusSummary{
			DeviceID:  bep.FormatDeviceID(pc.DeviceID),
			Name:      pc.Name,
			Address:   pc.Address,
			Connected: pc.Connected,
		})
	}
	return status
}
