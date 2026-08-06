package stream

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Record is a single entry in a partition log.
//
// Unlike protocol.Message (which is a transient queue item), a Record is
// immutable once appended: it owns a permanent, gap-free sequence number
// within its partition and is never mutated or deleted individually. Deletion
// happens only at segment granularity, driven by retention.
type Record struct {
	Seq       uint64            // monotonic, gap-free within the partition
	Timestamp int64             // UnixNano, assigned by the broker at append time
	Key       []byte            // partition key (e.g. conversation ID); may be empty
	Headers   map[string]string // small metadata; not indexed
	Payload   []byte            // opaque bytes — never parsed by the broker
	Flags     uint8
	// Epoch is the leader epoch under which this record was appended. It is the
	// only thing that lets a follower tell "records I am missing" apart from
	// "records the old leader accepted but the new one never had" — two states
	// that look identical when all you compare is sequence numbers. See
	// leaderEpochCache.
	//
	// Zero means a record written before epochs existed, or by a standalone
	// node that never took leadership.
	Epoch uint32
}

// Record flags.
const (
	// FlagTombstone marks a record that supersedes earlier records with the
	// same Key during compaction. Used for cursor/metadata streams.
	FlagTombstone uint8 = 1 << 0
	// FlagEphemeral marks a record that must never be persisted. It is only
	// ever seen in memory; the segment writer rejects it.
	FlagEphemeral uint8 = 1 << 1
	// FlagHasEpoch marks a record whose body carries a leader epoch. Older logs
	// have no epoch field at all, so the flag — not a format version number —
	// is what makes a record self-describing: a log written before epochs
	// existed stays readable, record by record, with no migration step.
	FlagHasEpoch uint8 = 1 << 2
)

// On-disk record layout, little-endian throughout:
//
//	[crc32:4][bodyLen:4][body:bodyLen]
//
// where body is:
//
//	[seq:8][ts:8][flags:1][keyLen:2][hdrLen:4][payloadLen:4][epoch:4?][key][hdr][payload]
//
// The CRC covers the body only. bodyLen is outside the CRC so a torn write can
// be detected by reading the length, then failing the checksum — the reader
// truncates at the first bad record rather than trusting the rest of the file.
//
// The epoch is optional and sits *after* the fixed header rather than beside
// the flags, so every pre-epoch offset in this layout is unchanged. A reader
// that knows about epochs handles both shapes; the alternative — inserting a
// field mid-header — would have made every existing segment unreadable.
const (
	recordFrameHeaderSize = 8  // crc32 + bodyLen
	recordBodyHeaderSize  = 27 // seq + ts + flags + keyLen + hdrLen + payloadLen
	recordEpochSize       = 4  // present only when FlagHasEpoch is set
)

// MaxRecordSize bounds a single encoded record body. Chat payloads are small;
// media travels by reference (a URL to object storage), not inline. The cap
// exists so a malformed length field cannot make the reader allocate wildly.
const MaxRecordSize = 8 << 20 // 8MB

var (
	// ErrCorruptRecord means the CRC did not match — the log is truncated here.
	ErrCorruptRecord = errors.New("stream: corrupt record")
	// ErrRecordTooLarge means the record exceeds MaxRecordSize.
	ErrRecordTooLarge = errors.New("stream: record too large")
)

// encodeHeaders serialises headers as a flat sequence of length-prefixed
// key/value pairs. JSON would be simpler but costs an allocation-heavy
// unmarshal on every read; chat fan-out reads the same record many times.
func encodeHeaders(h map[string]string) []byte {
	if len(h) == 0 {
		return nil
	}
	n := 0
	for k, v := range h {
		n += 4 + len(k) + len(v)
	}
	buf := make([]byte, 0, n)
	var lb [2]byte
	for k, v := range h {
		binary.LittleEndian.PutUint16(lb[:], uint16(len(k)))
		buf = append(buf, lb[:]...)
		buf = append(buf, k...)
		binary.LittleEndian.PutUint16(lb[:], uint16(len(v)))
		buf = append(buf, lb[:]...)
		buf = append(buf, v...)
	}
	return buf
}

func decodeHeaders(buf []byte) (map[string]string, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	h := make(map[string]string)
	for len(buf) > 0 {
		if len(buf) < 2 {
			return nil, ErrCorruptRecord
		}
		kl := int(binary.LittleEndian.Uint16(buf))
		buf = buf[2:]
		if len(buf) < kl+2 {
			return nil, ErrCorruptRecord
		}
		k := string(buf[:kl])
		buf = buf[kl:]
		vl := int(binary.LittleEndian.Uint16(buf))
		buf = buf[2:]
		if len(buf) < vl {
			return nil, ErrCorruptRecord
		}
		h[k] = string(buf[:vl])
		buf = buf[vl:]
	}
	return h, nil
}

// bodyHeaderSize returns this record's fixed-header footprint, which depends on
// whether it carries an epoch.
func (r *Record) bodyHeaderSize() int {
	if r.Epoch > 0 {
		return recordBodyHeaderSize + recordEpochSize
	}
	return recordBodyHeaderSize
}

// encodedSize returns the total on-disk footprint of the record.
func (r *Record) encodedSize() int {
	return recordFrameHeaderSize + r.bodyHeaderSize() +
		len(r.Key) + len(encodeHeaders(r.Headers)) + len(r.Payload)
}

// encode serialises the record into a self-contained frame.
func (r *Record) encode() ([]byte, error) {
	hdr := encodeHeaders(r.Headers)
	bodyHdr := r.bodyHeaderSize()
	bodyLen := bodyHdr + len(r.Key) + len(hdr) + len(r.Payload)
	if bodyLen > MaxRecordSize {
		return nil, fmt.Errorf("%w: %d bytes (max %d)", ErrRecordTooLarge, bodyLen, MaxRecordSize)
	}
	if len(r.Key) > 0xFFFF {
		return nil, fmt.Errorf("stream: key too long: %d bytes", len(r.Key))
	}

	buf := make([]byte, recordFrameHeaderSize+bodyLen)
	body := buf[recordFrameHeaderSize:]

	flags := r.Flags
	if r.Epoch > 0 {
		flags |= FlagHasEpoch
	} else {
		// Never advertise an epoch we are not about to write: the flag is what
		// the decoder trusts to size the header.
		flags &^= FlagHasEpoch
	}

	binary.LittleEndian.PutUint64(body[0:8], r.Seq)
	binary.LittleEndian.PutUint64(body[8:16], uint64(r.Timestamp))
	body[16] = flags
	binary.LittleEndian.PutUint16(body[17:19], uint16(len(r.Key)))
	binary.LittleEndian.PutUint32(body[19:23], uint32(len(hdr)))
	binary.LittleEndian.PutUint32(body[23:27], uint32(len(r.Payload)))
	if r.Epoch > 0 {
		binary.LittleEndian.PutUint32(body[27:31], r.Epoch)
	}

	off := bodyHdr
	off += copy(body[off:], r.Key)
	off += copy(body[off:], hdr)
	copy(body[off:], r.Payload)

	binary.LittleEndian.PutUint32(buf[0:4], crc32.ChecksumIEEE(body))
	binary.LittleEndian.PutUint32(buf[4:8], uint32(bodyLen))
	return buf, nil
}

// decodeRecordBody parses a record body whose CRC has already been verified.
func decodeRecordBody(body []byte) (*Record, error) {
	if len(body) < recordBodyHeaderSize {
		return nil, ErrCorruptRecord
	}
	flags := body[16]
	keyLen := int(binary.LittleEndian.Uint16(body[17:19]))
	hdrLen := int(binary.LittleEndian.Uint32(body[19:23]))
	payLen := int(binary.LittleEndian.Uint32(body[23:27]))

	bodyHdr := recordBodyHeaderSize
	var epoch uint32
	if flags&FlagHasEpoch != 0 {
		bodyHdr += recordEpochSize
		if len(body) < bodyHdr {
			return nil, ErrCorruptRecord
		}
		epoch = binary.LittleEndian.Uint32(body[27:31])
	}

	if bodyHdr+keyLen+hdrLen+payLen != len(body) {
		return nil, ErrCorruptRecord
	}

	off := bodyHdr
	// Copy out of the shared read buffer: callers retain records past the
	// lifetime of the buffer (they go into fan-out channels).
	key := append([]byte(nil), body[off:off+keyLen]...)
	off += keyLen
	headers, err := decodeHeaders(body[off : off+hdrLen])
	if err != nil {
		return nil, err
	}
	off += hdrLen
	payload := append([]byte(nil), body[off:off+payLen]...)

	return &Record{
		Seq:       binary.LittleEndian.Uint64(body[0:8]),
		Timestamp: int64(binary.LittleEndian.Uint64(body[8:16])),
		Flags:     flags &^ FlagHasEpoch,
		Epoch:     epoch,
		Key:       key,
		Headers:   headers,
		Payload:   payload,
	}, nil
}
