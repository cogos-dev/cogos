// gateway_client.go — WebSocket JSON-RPC client for the OpenClaw gateway.
//
// Implements the challenge-response handshake (protocol v3) with Ed25519 device
// identity auth, and exposes Send() and Agent() methods.
//
// Device identity is loaded from ~/.openclaw/identity/device.json (shared with
// the openclaw CLI). The device keypair is used to sign the connect payload,
// proving ownership and enabling scoped access.

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// ─── Types ──────────────────────────────────────────────────────────────────────

// GatewayClient connects to the OpenClaw gateway over WebSocket.
type GatewayClient struct {
	url    string // ws:// or wss:// URL
	token  string // auth token (from OPENCLAW_TOKEN)
	device *deviceIdentity
	conn   *websocket.Conn
	reqID  atomic.Int64
}

// deviceIdentity holds the Ed25519 keypair for gateway auth.
type deviceIdentity struct {
	DeviceID      string `json:"deviceId"`
	PublicKeyPEM  string `json:"publicKeyPem"`
	PrivateKeyPEM string `json:"privateKeyPem"`

	publicKeyRaw  []byte            // raw 32-byte Ed25519 public key
	privateKey    ed25519.PrivateKey // parsed private key
	publicKeyB64U string            // base64url of raw public key
}

// SendParams are the parameters for the gateway "send" method.
type SendParams struct {
	To             string `json:"to"`
	Message        string `json:"message"`
	Channel        string `json:"channel,omitempty"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// SendResult is the response from a successful "send" call.
type SendResult struct {
	RunID     string `json:"runId"`
	MessageID string `json:"messageId"`
	Channel   string `json:"channel"`
}

// AgentParams are the parameters for the gateway "agent" method.
type AgentParams struct {
	Message        string `json:"message"`
	AgentID        string `json:"agentId,omitempty"`
	To             string `json:"to,omitempty"`
	Deliver        bool   `json:"deliver,omitempty"`
	Channel        string `json:"channel,omitempty"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// AgentResult is the response from the gateway "agent" call.
type AgentResult struct {
	RunID      string `json:"runId"`
	Status     string `json:"status"` // "accepted", "ok", "error"
	AcceptedAt int64  `json:"acceptedAt,omitempty"`
}

// ─── Device Identity ────────────────────────────────────────────────────────────

// loadDeviceIdentity reads ~/.openclaw/identity/device.json and parses the keys.
func loadDeviceIdentity() (*deviceIdentity, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("user home: %w", err)
	}

	path := filepath.Join(home, ".openclaw", "identity", "device.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read device identity: %w", err)
	}

	var dev deviceIdentity
	if err := json.Unmarshal(data, &dev); err != nil {
		return nil, fmt.Errorf("parse device identity: %w", err)
	}

	// Parse private key PEM → ed25519.PrivateKey
	block, _ := pem.Decode([]byte(dev.PrivateKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("decode private key PEM")
	}
	privKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	edKey, ok := privKey.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not Ed25519")
	}
	dev.privateKey = edKey

	// Extract raw 32-byte public key from the ed25519 private key
	pubKey := edKey.Public().(ed25519.PublicKey)
	dev.publicKeyRaw = []byte(pubKey)
	dev.publicKeyB64U = base64URLEncode(dev.publicKeyRaw)

	// Verify deviceId = sha256(rawPubKey) hex
	hash := sha256.Sum256(dev.publicKeyRaw)
	derivedID := hex.EncodeToString(hash[:])
	if derivedID != dev.DeviceID {
		return nil, fmt.Errorf("device ID mismatch: derived %s vs stored %s", derivedID, dev.DeviceID)
	}

	return &dev, nil
}

// sign creates an Ed25519 signature of the payload, returned as base64url.
func (d *deviceIdentity) sign(payload string) string {
	sig := ed25519.Sign(d.privateKey, []byte(payload))
	return base64URLEncode(sig)
}

// base64URLEncode encodes bytes as base64url without padding (RFC 4648 §5).
func base64URLEncode(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// ─── Constructor ────────────────────────────────────────────────────────────────

// NewGatewayClient creates a client for the given gateway WebSocket URL and token.
// The url should be ws:// or wss://. If an http(s) URL is passed, it is converted.
func NewGatewayClient(rawURL, token string) *GatewayClient {
	wsURL := httpToWS(rawURL)
	return &GatewayClient{url: wsURL, token: token}
}

// httpToWS converts http(s) URLs to ws(s) URLs. Already-ws URLs pass through.
func httpToWS(u string) string {
	if strings.HasPrefix(u, "http://") {
		return "ws://" + u[7:]
	}
	if strings.HasPrefix(u, "https://") {
		return "wss://" + u[8:]
	}
	return u
}

// ─── Connection ─────────────────────────────────────────────────────────────────

// Connect dials the gateway and performs the challenge-response handshake
// with Ed25519 device identity auth.
//
// Protocol flow:
//  1. Dial ws://host
//  2. Read connect.challenge event (contains nonce)
//  3. Build device auth payload, sign with Ed25519
//  4. Write connect request with device identity + optional token
//  5. Read hello-ok response
func (g *GatewayClient) Connect(ctx context.Context) error {
	// Load device identity
	dev, err := loadDeviceIdentity()
	if err != nil {
		return fmt.Errorf("load device identity: %w", err)
	}
	g.device = dev

	conn, _, err := websocket.Dial(ctx, g.url, nil)
	if err != nil {
		return fmt.Errorf("gateway dial %s: %w", g.url, err)
	}
	conn.SetReadLimit(1 << 20) // 1 MiB
	g.conn = conn

	// 1. Read challenge event
	var challenge map[string]interface{}
	if err := wsjson.Read(ctx, g.conn, &challenge); err != nil {
		g.conn.CloseNow()
		return fmt.Errorf("read challenge: %w", err)
	}

	eventName, _ := challenge["event"].(string)
	if challenge["type"] != "event" || eventName != "connect.challenge" {
		g.conn.CloseNow()
		return fmt.Errorf("expected connect.challenge, got type=%v event=%v", challenge["type"], eventName)
	}

	payload, _ := challenge["payload"].(map[string]interface{})
	nonce, _ := payload["nonce"].(string)

	// 2. Build signed connect request
	clientID := "gateway-client"
	clientMode := "backend"
	role := "operator"
	scopes := []string{"operator.admin"}
	signedAtMs := time.Now().UnixMilli()

	// Build device auth payload: v2|deviceId|clientId|clientMode|role|scopes|signedAtMs|token|nonce
	scopeStr := strings.Join(scopes, ",")
	tokenStr := g.token
	authPayload := strings.Join([]string{
		"v2",
		dev.DeviceID,
		clientID,
		clientMode,
		role,
		scopeStr,
		strconv.FormatInt(signedAtMs, 10),
		tokenStr,
		nonce,
	}, "|")

	signature := dev.sign(authPayload)

	connectID := g.nextID("connect")
	params := map[string]interface{}{
		"minProtocol": 1,
		"maxProtocol": 3,
		"client": map[string]interface{}{
			"id":       clientID,
			"version":  Version,
			"platform": "darwin",
			"mode":     clientMode,
		},
		"role":   role,
		"scopes": scopes,
		"device": map[string]interface{}{
			"id":        dev.DeviceID,
			"publicKey": dev.publicKeyB64U,
			"signature": signature,
			"signedAt":  signedAtMs,
			"nonce":     nonce,
		},
	}
	if g.token != "" {
		params["auth"] = map[string]interface{}{
			"token": g.token,
		}
	}

	connectReq := map[string]interface{}{
		"type":   "req",
		"id":     connectID,
		"method": "connect",
		"params": params,
	}

	if err := wsjson.Write(ctx, g.conn, connectReq); err != nil {
		g.conn.CloseNow()
		return fmt.Errorf("write connect: %w", err)
	}

	// 3. Read response — skip any events until we get our connect response
	for {
		var resp map[string]interface{}
		if err := wsjson.Read(ctx, g.conn, &resp); err != nil {
			g.conn.CloseNow()
			return fmt.Errorf("read connect response: %w", err)
		}

		if resp["type"] != "res" {
			continue
		}
		if resp["id"] != connectID {
			g.conn.CloseNow()
			return fmt.Errorf("unexpected connect response id: %v", resp["id"])
		}

		ok, _ := resp["ok"].(bool)
		if !ok {
			errMap, _ := resp["error"].(map[string]interface{})
			msg := "unknown error"
			if errMap != nil {
				if m, ok := errMap["message"].(string); ok {
					msg = m
				}
			}
			g.conn.CloseNow()
			return fmt.Errorf("gateway connect rejected: %s", msg)
		}

		return nil
	}
}

// Close gracefully closes the WebSocket connection.
func (g *GatewayClient) Close() error {
	if g.conn == nil {
		return nil
	}
	return g.conn.Close(websocket.StatusNormalClosure, "done")
}

// ─── RPC Methods ────────────────────────────────────────────────────────────────

// Send posts a message to a channel through the gateway.
func (g *GatewayClient) Send(ctx context.Context, p SendParams) (*SendResult, error) {
	resp, err := g.call(ctx, "send", p)
	if err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	result := &SendResult{}
	if runID, ok := resp["runId"].(string); ok {
		result.RunID = runID
	}
	if msgID, ok := resp["messageId"].(string); ok {
		result.MessageID = msgID
	}
	if ch, ok := resp["channel"].(string); ok {
		result.Channel = ch
	}
	return result, nil
}

// Agent triggers an agent session through the gateway.
// Returns immediately with an "accepted" status; the agent runs asynchronously.
func (g *GatewayClient) Agent(ctx context.Context, p AgentParams) (*AgentResult, error) {
	resp, err := g.call(ctx, "agent", p)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}

	result := &AgentResult{}
	if runID, ok := resp["runId"].(string); ok {
		result.RunID = runID
	}
	if status, ok := resp["status"].(string); ok {
		result.Status = status
	}
	if at, ok := resp["acceptedAt"].(float64); ok {
		result.AcceptedAt = int64(at)
	}
	return result, nil
}

// ─── Internals ──────────────────────────────────────────────────────────────────

// call sends a JSON-RPC request and reads the response.
func (g *GatewayClient) call(ctx context.Context, method string, params interface{}) (map[string]interface{}, error) {
	if g.conn == nil {
		return nil, fmt.Errorf("not connected")
	}

	id := g.nextID(method)
	req := map[string]interface{}{
		"type":   "req",
		"id":     id,
		"method": method,
		"params": params,
	}

	if err := wsjson.Write(ctx, g.conn, req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	// Read response — skip intervening events until we get our response ID.
	for {
		var resp map[string]interface{}
		if err := wsjson.Read(ctx, g.conn, &resp); err != nil {
			return nil, fmt.Errorf("read %s response: %w", method, err)
		}

		if resp["type"] == "res" && resp["id"] == id {
			ok, _ := resp["ok"].(bool)
			if !ok {
				errMap, _ := resp["error"].(map[string]interface{})
				msg := "unknown error"
				code := ""
				if errMap != nil {
					if m, ok := errMap["message"].(string); ok {
						msg = m
					}
					if c, ok := errMap["code"].(string); ok {
						code = c
					}
				}
				if code != "" {
					return nil, fmt.Errorf("%s [%s]: %s", method, code, msg)
				}
				return nil, fmt.Errorf("%s: %s", method, msg)
			}

			payload, _ := resp["payload"].(map[string]interface{})
			if payload == nil {
				payload = make(map[string]interface{})
			}
			return payload, nil
		}
		// Skip events and other messages while waiting for our response
	}
}

// nextID generates a unique request ID for the given method.
func (g *GatewayClient) nextID(method string) string {
	n := g.reqID.Add(1)
	return fmt.Sprintf("%s-%d", method, n)
}

