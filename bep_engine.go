// bep_engine.go — BEPEngine: TLS listener, peer dialer, connection management,
// and BEP protocol lifecycle. Gated on cluster.enabled in serve.go.
//
// Integration pattern (matches ToolRouter/DiscordBridge/BEPProvider):
//   Create → Configure callbacks → Start() → defer Stop()
//
// Per-connection: TLS handshake → Hello → ClusterConfig → Index → steady-state
// message loop with 90s Ping interval.

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sync"
	"time"
)

// ─── BEPEngine ──────────────────────────────────────────────────────────────────

// BEPEngine manages BEP transport for cross-node agent CRD sync.
type BEPEngine struct {
	mu sync.Mutex

	root      string
	config    *BEPConfig
	deviceID  DeviceID
	shortID   uint64
	tlsConfig *tls.Config
	tlsCert   tls.Certificate
	listener  net.Listener

	provider *BEPProvider
	model    *AgentSyncModel
	bus      *busSessionManager

	peers    map[DeviceID]*PeerConnection
	peersMu  sync.RWMutex

	stopCh   chan struct{}
	stopped  bool
	wg       sync.WaitGroup
}

// PeerConnection represents an active connection to a peer node.
type PeerConnection struct {
	DeviceID  DeviceID
	Name      string
	Address   string
	Wire      *BEPWire
	Connected bool
	LastPing  time.Time
	closeCh   chan struct{}
	closeOnce sync.Once
}

// ─── Constructor ────────────────────────────────────────────────────────────────

// NewBEPEngine creates a new engine from cluster config.
// Does not start listening or connecting — call Start() for that.
func NewBEPEngine(root string, config *BEPConfig, provider *BEPProvider) (*BEPEngine, error) {
	// Resolve cert directory.
	certDir := ExpandCertDir(config.CertDir)

	// Load TLS certificate.
	cert, err := LoadBEPCert(certDir)
	if err != nil {
		return nil, fmt.Errorf("load BEP cert: %w", err)
	}

	// Derive DeviceID.
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse cert: %w", err)
	}
	deviceID := DeviceIDFromCert(x509Cert)
	shortID := ShortIDFromDeviceID(deviceID)

	// Build trusted peers set.
	trustedPeers := make(map[DeviceID]bool)
	for _, p := range config.Peers {
		if p.Trusted {
			pid, err := ParseDeviceID(p.DeviceID)
			if err != nil {
				log.Printf("[bep-engine] invalid peer device ID %q: %v", p.DeviceID, err)
				continue
			}
			trustedPeers[pid] = true
		}
	}

	tlsConfig := BEPTLSConfig(cert, func(id DeviceID) bool {
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
		peers:     make(map[DeviceID]*PeerConnection),
		stopCh:    make(chan struct{}),
	}

	// Create sync model.
	stateDir := filepath.Join(root, ".cog", ".state", "bep")
	engine.model = NewAgentSyncModel(engine, provider.watchDir, stateDir, shortID)

	return engine, nil
}

// ─── Lifecycle ──────────────────────────────────────────────────────────────────

// SetBus sets the bus session manager for event forwarding.
func (e *BEPEngine) SetBus(bus *busSessionManager) {
	e.bus = bus
	e.model.SetEventEmitter(func(evt SyncEvent) {
		evt.Timestamp = time.Now().UTC().Format(time.RFC3339)
		EmitSyncEvent(evt.Type, evt.Summary)
		if e.bus != nil {
			// Forward to bus — handlers registered on the bus will receive it.
			busEvt := SyncEventToBusData(evt)
			busEvt.From = "bep-engine"
			e.bus.mu.Lock()
			handlers := make([]busEventHandler, len(e.bus.eventHandlers))
			copy(handlers, e.bus.eventHandlers)
			e.bus.mu.Unlock()
			for _, h := range handlers {
				h.handler("bep-engine", busEvt)
			}
		}
	})
}

// Start begins listening for incoming connections and dials configured peers.
func (e *BEPEngine) Start() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// ListenPort 0 means OS-assigned random port (useful for tests).
	// Default 22000 is set at config creation time (cluster init / cluster.yaml).
	addr := fmt.Sprintf(":%d", e.config.ListenPort)
	ln, err := tls.Listen("tcp", addr, e.tlsConfig)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	e.listener = ln

	log.Printf("[bep-engine] started, device=%s listen=%s peers=%d",
		FormatDeviceID(e.deviceID)[:7], ln.Addr(), len(e.config.Peers))

	EmitEngineStarted(FormatDeviceID(e.deviceID), ln.Addr().String(), len(e.config.Peers))

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
	EmitEngineStopped("shutdown")
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

func (e *BEPEngine) runDialer(peer BEPPeer) {
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
		peerID, err := ParseDeviceID(peer.DeviceID)
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
	wire := NewBEPWire(conn)
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
	peerID := DeviceIDFromCert(state.PeerCertificates[0])
	peerShort := FormatDeviceID(peerID)[:7]

	direction := "inbound"
	if !inbound {
		direction = "outbound"
	}
	log.Printf("[bep-engine] %s connection from %s (%s)", direction, peerShort, conn.RemoteAddr())

	// 1. Hello exchange.
	deviceName := FormatDeviceID(e.deviceID)[:7]
	if e.config.NodeName != "" {
		deviceName = e.config.NodeName
	}
	hello := &BEPHello{
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
	cc := &BEPClusterConfig{
		Folders: []*BEPFolder{{
			ID:    agentSyncFolderID,
			Label: "Agent Definitions",
			Devices: []*BEPDevice{
				{ID: e.deviceID[:], Name: hello.DeviceName},
				{ID: peerID[:], Name: peerHello.DeviceName},
			},
		}},
	}
	if err := wire.WriteMessage(MessageTypeClusterConfig, cc.Marshal()); err != nil {
		log.Printf("[bep-engine] failed to send cluster config to %s: %v", peerShort, err)
		return
	}

	_, ccPayload, err := wire.ReadMessage()
	if err != nil {
		log.Printf("[bep-engine] failed to read cluster config from %s: %v", peerShort, err)
		return
	}
	peerCC := &BEPClusterConfig{}
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

	EmitPeerConnected(peerShort, peerHello.DeviceName)

	defer func() {
		e.peersMu.Lock()
		if e.peers[peerID] == pc {
			delete(e.peers, peerID)
		}
		e.peersMu.Unlock()
		EmitPeerDisconnected(peerShort, "connection closed")
	}()

	// 4. Send full Index.
	files := e.model.LoadAndScanIndex()
	idx := &BEPIndex{
		Folder: agentSyncFolderID,
		Files:  files,
	}
	if err := wire.WriteMessage(MessageTypeIndex, idx.Marshal()); err != nil {
		log.Printf("[bep-engine] failed to send index to %s: %v", peerShort, err)
		return
	}

	// 5. Steady-state message loop.
	e.runPeerLoop(pc, peerID)
}

// ─── Peer message loop ──────────────────────────────────────────────────────────

func (e *BEPEngine) runPeerLoop(pc *PeerConnection, peerID DeviceID) {
	peerShort := FormatDeviceID(peerID)[:7]

	// Ping ticker: send Ping every 90 seconds.
	pingTicker := time.NewTicker(90 * time.Second)
	defer pingTicker.Stop()

	// Read messages in a goroutine.
	msgCh := make(chan struct {
		msgType MessageType
		payload []byte
		err     error
	}, 1)

	go func() {
		for {
			msgType, payload, err := pc.Wire.ReadMessage()
			select {
			case msgCh <- struct {
				msgType MessageType
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
			ping := &BEPPing{}
			if err := pc.Wire.WriteMessage(MessageTypePing, ping.Marshal()); err != nil {
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
			case MessageTypeIndex:
				idx := &BEPIndex{}
				if err := idx.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad index from %s: %v", peerShort, err)
					continue
				}
				e.model.HandleIndex(peerID, idx.Files)

			case MessageTypeIndexUpdate:
				update := &BEPIndex{}
				if err := update.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad index update from %s: %v", peerShort, err)
					continue
				}
				e.model.HandleIndexUpdate(peerID, update.Files)

			case MessageTypeRequest:
				req := &BEPRequest{}
				if err := req.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad request from %s: %v", peerShort, err)
					continue
				}
				resp := e.model.HandleRequest(req)
				if err := pc.Wire.WriteMessage(MessageTypeResponse, resp.Marshal()); err != nil {
					log.Printf("[bep-engine] failed to send response to %s: %v", peerShort, err)
					return
				}

			case MessageTypeResponse:
				resp := &BEPResponse{}
				if err := resp.Unmarshal(msg.payload); err != nil {
					log.Printf("[bep-engine] bad response from %s: %v", peerShort, err)
					continue
				}
				e.model.HandleResponse(resp)

			case MessageTypePing:
				// Respond with Ping.
				pong := &BEPPing{}
				if err := pc.Wire.WriteMessage(MessageTypePing, pong.Marshal()); err != nil {
					log.Printf("[bep-engine] pong to %s failed: %v", peerShort, err)
					return
				}

			case MessageTypeClose:
				cl := &BEPClose{}
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
func (e *BEPEngine) SendToPeer(peerID DeviceID, msgType MessageType, payload []byte) {
	e.peersMu.RLock()
	pc, ok := e.peers[peerID]
	e.peersMu.RUnlock()
	if !ok {
		return
	}
	if err := pc.Wire.WriteMessage(msgType, payload); err != nil {
		log.Printf("[bep-engine] send to %s failed: %v", FormatDeviceID(peerID)[:7], err)
	}
}

// BroadcastMessage sends a message to all connected peers.
func (e *BEPEngine) BroadcastMessage(msgType MessageType, payload []byte) {
	e.peersMu.RLock()
	peers := make([]*PeerConnection, 0, len(e.peers))
	for _, pc := range e.peers {
		peers = append(peers, pc)
	}
	e.peersMu.RUnlock()

	for _, pc := range peers {
		if err := pc.Wire.WriteMessage(msgType, payload); err != nil {
			log.Printf("[bep-engine] broadcast to %s failed: %v",
				FormatDeviceID(pc.DeviceID)[:7], err)
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
			cl := &BEPClose{Reason: "shutdown"}
			_ = pc.Wire.WriteMessage(MessageTypeClose, cl.Marshal())
			pc.Wire.Close()
		}
	})
}

// ─── Status ─────────────────────────────────────────────────────────────────────

// EngineStatus returns the current engine status for CLI/API.
type EngineStatus struct {
	Running    bool                `json:"running"`
	DeviceID   string              `json:"device_id"`
	ListenAddr string              `json:"listen_addr"`
	PeerCount  int                 `json:"peer_count"`
	Peers      []PeerStatusSummary `json:"peers"`
}

type PeerStatusSummary struct {
	DeviceID  string `json:"device_id"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Connected bool   `json:"connected"`
}

// Status returns the current engine status.
func (e *BEPEngine) Status() EngineStatus {
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()

	status := EngineStatus{
		Running:  true,
		DeviceID: FormatDeviceID(e.deviceID),
	}
	if e.listener != nil {
		status.ListenAddr = e.listener.Addr().String()
	}
	status.PeerCount = len(e.peers)

	for _, pc := range e.peers {
		status.Peers = append(status.Peers, PeerStatusSummary{
			DeviceID:  FormatDeviceID(pc.DeviceID),
			Name:      pc.Name,
			Address:   pc.Address,
			Connected: pc.Connected,
		})
	}
	return status
}
