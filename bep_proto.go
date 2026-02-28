// bep_proto.go — BEP v1 message types with manual protowire encoding.
// Field numbers match bep_proto/bep.proto for wire compatibility with Syncthing.
// Uses google.golang.org/protobuf/encoding/protowire (already in go.mod).

package main

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// ─── Message type enum ──────────────────────────────────────────────────────────

type MessageType int32

const (
	MessageTypeClusterConfig MessageType = 0
	MessageTypeIndex         MessageType = 1
	MessageTypeIndexUpdate   MessageType = 2
	MessageTypeRequest       MessageType = 3
	MessageTypeResponse      MessageType = 4
	MessageTypePing          MessageType = 6
	MessageTypeClose         MessageType = 7
)

type MessageCompression int32

const (
	CompressionNone MessageCompression = 0
)

type ErrorCode int32

const (
	ErrorCodeNoError     ErrorCode = 0
	ErrorCodeGeneric     ErrorCode = 1
	ErrorCodeNoSuchFile  ErrorCode = 2
	ErrorCodeInvalidFile ErrorCode = 3
)

// BEP Hello magic number (0x2EA7D90B).
const BEPMagic uint32 = 0x2EA7D90B

// ─── protowire helpers ──────────────────────────────────────────────────────────

type pbEncoder struct{ buf []byte }

func (e *pbEncoder) varint(field protowire.Number, v uint64) {
	if v == 0 {
		return
	}
	e.buf = protowire.AppendTag(e.buf, field, protowire.VarintType)
	e.buf = protowire.AppendVarint(e.buf, v)
}

func (e *pbEncoder) sint64(field protowire.Number, v int64) {
	e.varint(field, uint64(v))
}

func (e *pbEncoder) boolean(field protowire.Number, v bool) {
	if !v {
		return
	}
	e.varint(field, 1)
}

func (e *pbEncoder) str(field protowire.Number, s string) {
	if s == "" {
		return
	}
	e.buf = protowire.AppendTag(e.buf, field, protowire.BytesType)
	e.buf = protowire.AppendString(e.buf, s)
}

func (e *pbEncoder) bytes(field protowire.Number, b []byte) {
	if len(b) == 0 {
		return
	}
	e.buf = protowire.AppendTag(e.buf, field, protowire.BytesType)
	e.buf = protowire.AppendBytes(e.buf, b)
}

func (e *pbEncoder) msg(field protowire.Number, data []byte) {
	if len(data) == 0 {
		return
	}
	e.buf = protowire.AppendTag(e.buf, field, protowire.BytesType)
	e.buf = protowire.AppendBytes(e.buf, data)
}

// pbDecode iterates protobuf fields. Calls fn(fieldNum, wireType, fieldData).
// For varint fields, fieldData encodes the varint value.
func pbDecode(b []byte, fn func(protowire.Number, protowire.Type, []byte) error) error {
	for len(b) > 0 {
		num, wtype, n := protowire.ConsumeTag(b)
		if n < 0 {
			return errors.New("bep: bad tag")
		}
		b = b[n:]

		var val []byte
		switch wtype {
		case protowire.VarintType:
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return errors.New("bep: bad varint")
			}
			// Encode varint as 8 bytes for convenience.
			val = make([]byte, 8)
			val[0] = byte(v)
			val[1] = byte(v >> 8)
			val[2] = byte(v >> 16)
			val[3] = byte(v >> 24)
			val[4] = byte(v >> 32)
			val[5] = byte(v >> 40)
			val[6] = byte(v >> 48)
			val[7] = byte(v >> 56)
			b = b[vn:]
		case protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return errors.New("bep: bad bytes")
			}
			val = v
			b = b[vn:]
		case protowire.Fixed32Type:
			_, vn := protowire.ConsumeFixed32(b)
			if vn < 0 {
				return errors.New("bep: bad fixed32")
			}
			b = b[vn:]
			continue // skip unknown fixed32
		case protowire.Fixed64Type:
			_, vn := protowire.ConsumeFixed64(b)
			if vn < 0 {
				return errors.New("bep: bad fixed64")
			}
			b = b[vn:]
			continue // skip unknown fixed64
		default:
			return fmt.Errorf("bep: unknown wire type %d", wtype)
		}

		if err := fn(num, wtype, val); err != nil {
			return err
		}
	}
	return nil
}

// decodeVarint extracts uint64 from the 8-byte LE encoding used by pbDecode.
func decodeVarint(val []byte) uint64 {
	if len(val) < 8 {
		return 0
	}
	return uint64(val[0]) | uint64(val[1])<<8 | uint64(val[2])<<16 | uint64(val[3])<<24 |
		uint64(val[4])<<32 | uint64(val[5])<<40 | uint64(val[6])<<48 | uint64(val[7])<<56
}

// ─── Hello (field numbers match bep.proto) ──────────────────────────────────────

type BEPHello struct {
	DeviceName    string // field 1
	ClientName    string // field 2
	ClientVersion string // field 3
}

func (h *BEPHello) Marshal() []byte {
	var e pbEncoder
	e.str(1, h.DeviceName)
	e.str(2, h.ClientName)
	e.str(3, h.ClientVersion)
	return e.buf
}

func (h *BEPHello) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			h.DeviceName = string(val)
		case 2:
			h.ClientName = string(val)
		case 3:
			h.ClientVersion = string(val)
		}
		return nil
	})
}

// ─── Header ─────────────────────────────────────────────────────────────────────

type BEPHeader struct {
	Type        MessageType        // field 1
	Compression MessageCompression // field 2
}

func (h *BEPHeader) Marshal() []byte {
	var e pbEncoder
	e.varint(1, uint64(h.Type))
	e.varint(2, uint64(h.Compression))
	return e.buf
}

func (h *BEPHeader) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			h.Type = MessageType(decodeVarint(val))
		case 2:
			h.Compression = MessageCompression(decodeVarint(val))
		}
		return nil
	})
}

// ─── ClusterConfig / Folder / Device ────────────────────────────────────────────

type BEPDevice struct {
	ID   []byte // field 1 — 32-byte DeviceID
	Name string // field 2
}

func (d *BEPDevice) Marshal() []byte {
	var e pbEncoder
	e.bytes(1, d.ID)
	e.str(2, d.Name)
	return e.buf
}

func (d *BEPDevice) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			d.ID = append([]byte(nil), val...)
		case 2:
			d.Name = string(val)
		}
		return nil
	})
}

type BEPFolder struct {
	ID      string       // field 1
	Label   string       // field 2
	Devices []*BEPDevice // field 3 (repeated)
}

func (f *BEPFolder) Marshal() []byte {
	var e pbEncoder
	e.str(1, f.ID)
	e.str(2, f.Label)
	for _, d := range f.Devices {
		e.msg(3, d.Marshal())
	}
	return e.buf
}

func (f *BEPFolder) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			f.ID = string(val)
		case 2:
			f.Label = string(val)
		case 3:
			d := &BEPDevice{}
			if err := d.Unmarshal(val); err != nil {
				return err
			}
			f.Devices = append(f.Devices, d)
		}
		return nil
	})
}

type BEPClusterConfig struct {
	Folders []*BEPFolder // field 1 (repeated)
}

func (c *BEPClusterConfig) Marshal() []byte {
	var e pbEncoder
	for _, f := range c.Folders {
		e.msg(1, f.Marshal())
	}
	return e.buf
}

func (c *BEPClusterConfig) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		if num == 1 {
			f := &BEPFolder{}
			if err := f.Unmarshal(val); err != nil {
				return err
			}
			c.Folders = append(c.Folders, f)
		}
		return nil
	})
}

// ─── Counter / Vector (version vectors) ─────────────────────────────────────────

type BEPCounter struct {
	ID    uint64 // field 1 — short device ID
	Value uint64 // field 2
}

func (c *BEPCounter) Marshal() []byte {
	var e pbEncoder
	e.varint(1, c.ID)
	e.varint(2, c.Value)
	return e.buf
}

func (c *BEPCounter) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			c.ID = decodeVarint(val)
		case 2:
			c.Value = decodeVarint(val)
		}
		return nil
	})
}

type BEPVector struct {
	Counters []*BEPCounter // field 1 (repeated)
}

func (v *BEPVector) Marshal() []byte {
	var e pbEncoder
	for _, c := range v.Counters {
		e.msg(1, c.Marshal())
	}
	return e.buf
}

func (v *BEPVector) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		if num == 1 {
			c := &BEPCounter{}
			if err := c.Unmarshal(val); err != nil {
				return err
			}
			v.Counters = append(v.Counters, c)
		}
		return nil
	})
}

// ─── BlockInfo ──────────────────────────────────────────────────────────────────

type BEPBlockInfo struct {
	Offset   int64  // field 1
	Size     int32  // field 2
	Hash     []byte // field 3
	WeakHash uint32 // field 4
}

func (bi *BEPBlockInfo) Marshal() []byte {
	var e pbEncoder
	e.sint64(1, bi.Offset)
	e.varint(2, uint64(bi.Size))
	e.bytes(3, bi.Hash)
	e.varint(4, uint64(bi.WeakHash))
	return e.buf
}

func (bi *BEPBlockInfo) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			bi.Offset = int64(decodeVarint(val))
		case 2:
			bi.Size = int32(decodeVarint(val))
		case 3:
			bi.Hash = append([]byte(nil), val...)
		case 4:
			bi.WeakHash = uint32(decodeVarint(val))
		}
		return nil
	})
}

// ─── FileInfo ───────────────────────────────────────────────────────────────────

type BEPFileInfo struct {
	Name       string          // field 1
	Size       int64           // field 3
	ModifiedS  int64           // field 5
	ModifiedNs int32           // field 6
	ModifiedBy uint64          // field 8
	Deleted    bool            // field 9
	Version    BEPVector       // field 12
	Sequence   int64           // field 13
	Blocks     []*BEPBlockInfo // field 16 (repeated)
	BlocksHash []byte          // field 18
}

func (fi *BEPFileInfo) Marshal() []byte {
	var e pbEncoder
	e.str(1, fi.Name)
	// field 2: type (always 0 = FILE for agent CRDs)
	e.sint64(3, fi.Size)
	// field 4: permissions (unused)
	e.sint64(5, fi.ModifiedS)
	e.varint(6, uint64(fi.ModifiedNs))
	e.varint(8, fi.ModifiedBy)
	e.boolean(9, fi.Deleted)
	// fields 10-11: invalid, no_permissions (unused)
	if vb := fi.Version.Marshal(); len(vb) > 0 {
		e.msg(12, vb)
	}
	e.sint64(13, fi.Sequence)
	for _, bi := range fi.Blocks {
		e.msg(16, bi.Marshal())
	}
	e.bytes(18, fi.BlocksHash)
	return e.buf
}

func (fi *BEPFileInfo) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, wtype protowire.Type, val []byte) error {
		switch num {
		case 1:
			fi.Name = string(val)
		case 3:
			fi.Size = int64(decodeVarint(val))
		case 5:
			fi.ModifiedS = int64(decodeVarint(val))
		case 6:
			fi.ModifiedNs = int32(decodeVarint(val))
		case 8:
			fi.ModifiedBy = decodeVarint(val)
		case 9:
			fi.Deleted = decodeVarint(val) != 0
		case 12:
			if err := fi.Version.Unmarshal(val); err != nil {
				return err
			}
		case 13:
			fi.Sequence = int64(decodeVarint(val))
		case 16:
			bi := &BEPBlockInfo{}
			if err := bi.Unmarshal(val); err != nil {
				return err
			}
			fi.Blocks = append(fi.Blocks, bi)
		case 18:
			fi.BlocksHash = append([]byte(nil), val...)
		}
		return nil
	})
}

// ─── Index / IndexUpdate ────────────────────────────────────────────────────────

type BEPIndex struct {
	Folder string         // field 1
	Files  []*BEPFileInfo // field 2 (repeated)
}

func (idx *BEPIndex) Marshal() []byte {
	var e pbEncoder
	e.str(1, idx.Folder)
	for _, f := range idx.Files {
		e.msg(2, f.Marshal())
	}
	return e.buf
}

func (idx *BEPIndex) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			idx.Folder = string(val)
		case 2:
			fi := &BEPFileInfo{}
			if err := fi.Unmarshal(val); err != nil {
				return err
			}
			idx.Files = append(idx.Files, fi)
		}
		return nil
	})
}

// BEPIndexUpdate has the same wire format as Index.
type BEPIndexUpdate = BEPIndex

// ─── Request / Response ─────────────────────────────────────────────────────────

type BEPRequest struct {
	ID     int32  // field 1
	Folder string // field 2
	Name   string // field 3
	Offset int64  // field 4
	Size   int32  // field 5
	Hash   []byte // field 6
}

func (r *BEPRequest) Marshal() []byte {
	var e pbEncoder
	e.varint(1, uint64(r.ID))
	e.str(2, r.Folder)
	e.str(3, r.Name)
	e.sint64(4, r.Offset)
	e.varint(5, uint64(r.Size))
	e.bytes(6, r.Hash)
	return e.buf
}

func (r *BEPRequest) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			r.ID = int32(decodeVarint(val))
		case 2:
			r.Folder = string(val)
		case 3:
			r.Name = string(val)
		case 4:
			r.Offset = int64(decodeVarint(val))
		case 5:
			r.Size = int32(decodeVarint(val))
		case 6:
			r.Hash = append([]byte(nil), val...)
		}
		return nil
	})
}

type BEPResponse struct {
	ID   int32     // field 1
	Data []byte    // field 2
	Code ErrorCode // field 3
}

func (r *BEPResponse) Marshal() []byte {
	var e pbEncoder
	e.varint(1, uint64(r.ID))
	e.bytes(2, r.Data)
	e.varint(3, uint64(r.Code))
	return e.buf
}

func (r *BEPResponse) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		switch num {
		case 1:
			r.ID = int32(decodeVarint(val))
		case 2:
			r.Data = append([]byte(nil), val...)
		case 3:
			r.Code = ErrorCode(decodeVarint(val))
		}
		return nil
	})
}

// ─── Ping / Close ───────────────────────────────────────────────────────────────

type BEPPing struct{}

func (p *BEPPing) Marshal() []byte  { return nil }
func (p *BEPPing) Unmarshal([]byte) error { return nil }

type BEPClose struct {
	Reason string // field 1
}

func (c *BEPClose) Marshal() []byte {
	var e pbEncoder
	e.str(1, c.Reason)
	return e.buf
}

func (c *BEPClose) Unmarshal(b []byte) error {
	return pbDecode(b, func(num protowire.Number, _ protowire.Type, val []byte) error {
		if num == 1 {
			c.Reason = string(val)
		}
		return nil
	})
}
