// bep_wire.go — BEP framing layer.
// Hello exchange: [4-byte magic 0x2EA7D90B][2-byte length BE][Hello protobuf]
// Messages:       [4-byte header-len BE][Header protobuf][4-byte msg-len BE][Message protobuf]
//
// All lengths are big-endian. Maximum message size: 64 MiB (BEP spec limit).

package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

const (
	bepMaxMessageSize = 64 << 20 // 64 MiB
	bepMaxHelloSize   = 32768    // 32 KiB
)

// BEPWire handles reading and writing BEP-framed messages on a connection.
type BEPWire struct {
	conn   io.ReadWriteCloser
	readMu sync.Mutex
	writeMu sync.Mutex
}

// NewBEPWire wraps a connection with BEP framing.
func NewBEPWire(conn io.ReadWriteCloser) *BEPWire {
	return &BEPWire{conn: conn}
}

// ─── Hello exchange ─────────────────────────────────────────────────────────────

// WriteHello sends a BEP Hello message with magic prefix.
func (w *BEPWire) WriteHello(h *BEPHello) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	payload := h.Marshal()
	if len(payload) > bepMaxHelloSize {
		return fmt.Errorf("hello too large: %d > %d", len(payload), bepMaxHelloSize)
	}

	// [4-byte magic][2-byte length][hello protobuf]
	buf := make([]byte, 6+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], BEPMagic)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(payload)))
	copy(buf[6:], payload)

	_, err := w.conn.Write(buf)
	return err
}

// ReadHello reads a BEP Hello message, verifying the magic prefix.
func (w *BEPWire) ReadHello() (*BEPHello, error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()

	// Read magic + length.
	hdr := make([]byte, 6)
	if _, err := io.ReadFull(w.conn, hdr); err != nil {
		return nil, fmt.Errorf("read hello header: %w", err)
	}

	magic := binary.BigEndian.Uint32(hdr[0:4])
	if magic != BEPMagic {
		return nil, fmt.Errorf("bad magic: 0x%08X (want 0x%08X)", magic, BEPMagic)
	}

	length := binary.BigEndian.Uint16(hdr[4:6])
	if int(length) > bepMaxHelloSize {
		return nil, fmt.Errorf("hello too large: %d", length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(w.conn, payload); err != nil {
		return nil, fmt.Errorf("read hello payload: %w", err)
	}

	h := &BEPHello{}
	if err := h.Unmarshal(payload); err != nil {
		return nil, fmt.Errorf("unmarshal hello: %w", err)
	}
	return h, nil
}

// ─── Message exchange ───────────────────────────────────────────────────────────

// WriteMessage sends a typed BEP message with header framing.
func (w *BEPWire) WriteMessage(msgType MessageType, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	// Encode header.
	header := &BEPHeader{Type: msgType, Compression: CompressionNone}
	headerBytes := header.Marshal()

	// Build frame: [4-byte header-len][header][4-byte msg-len][message]
	frame := make([]byte, 4+len(headerBytes)+4+len(payload))
	binary.BigEndian.PutUint32(frame[0:4], uint32(len(headerBytes)))
	copy(frame[4:4+len(headerBytes)], headerBytes)
	off := 4 + len(headerBytes)
	binary.BigEndian.PutUint32(frame[off:off+4], uint32(len(payload)))
	copy(frame[off+4:], payload)

	_, err := w.conn.Write(frame)
	return err
}

// ReadMessage reads a typed BEP message from the connection.
// Returns the message type and raw protobuf payload.
func (w *BEPWire) ReadMessage() (MessageType, []byte, error) {
	w.readMu.Lock()
	defer w.readMu.Unlock()

	// Read header length.
	var hdrLen [4]byte
	if _, err := io.ReadFull(w.conn, hdrLen[:]); err != nil {
		return 0, nil, fmt.Errorf("read header length: %w", err)
	}
	hl := binary.BigEndian.Uint32(hdrLen[:])
	if hl > bepMaxMessageSize {
		return 0, nil, fmt.Errorf("header too large: %d", hl)
	}

	// Read header.
	headerBytes := make([]byte, hl)
	if hl > 0 {
		if _, err := io.ReadFull(w.conn, headerBytes); err != nil {
			return 0, nil, fmt.Errorf("read header: %w", err)
		}
	}
	header := &BEPHeader{}
	if err := header.Unmarshal(headerBytes); err != nil {
		return 0, nil, fmt.Errorf("unmarshal header: %w", err)
	}

	// Read message length.
	var msgLen [4]byte
	if _, err := io.ReadFull(w.conn, msgLen[:]); err != nil {
		return 0, nil, fmt.Errorf("read message length: %w", err)
	}
	ml := binary.BigEndian.Uint32(msgLen[:])
	if ml > bepMaxMessageSize {
		return 0, nil, fmt.Errorf("message too large: %d", ml)
	}

	// Read message payload.
	payload := make([]byte, ml)
	if ml > 0 {
		if _, err := io.ReadFull(w.conn, payload); err != nil {
			return 0, nil, fmt.Errorf("read message payload: %w", err)
		}
	}

	return header.Type, payload, nil
}

// Close closes the underlying connection.
func (w *BEPWire) Close() error {
	return w.conn.Close()
}
