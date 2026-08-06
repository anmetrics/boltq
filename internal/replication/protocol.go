// Package replication copies the stream log from a leader to followers.
//
// Until now a message existed only on the node that accepted it: losing that
// node's disk lost the messages. This package closes that gap for the write
// path — a leader ships every record to its followers and can be made to wait
// for them before acknowledging a publisher, so an acknowledged message
// survives the loss of one machine.
//
// What it does NOT yet do is choose leaders. Which node leads which partition
// is configuration, not consensus; promotion is a manual, one-at-a-time
// procedure. Automatic failover needs a metadata layer that can agree on
// leadership under partition, and this package does not have one.
//
// It does, however, now detect the damage a bad promotion causes. Every record
// carries the leader epoch it was written under, and a reconnecting follower
// asks the leader where its own epoch ended before fetching anything. Records
// past that point were accepted by a leader the cluster never agreed on, and
// are truncated away rather than left to sit at a sequence the new leader has
// assigned to something else. See stream.leaderEpochCache.
//
// That ordering is deliberate: detection before automation. Automatic
// promotion built on top of epoch reconciliation is safe; automatic promotion
// without it silently forks the log.
package replication

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire protocol.
//
// Every message is [type:1][length:4 LE][payload]. Record payloads reuse the
// stream package's own encoding, checksum included, so a record that crosses
// the network is byte-identical to the one on the leader's disk.
const (
	// MsgHello is the follower announcing itself. Payload: node ID.
	MsgHello byte = 0x01
	// MsgFetch asks for a partition from a sequence onwards.
	MsgFetch byte = 0x02
	// MsgRecords carries a batch of records for one partition.
	MsgRecords byte = 0x03
	// MsgAck reports a follower's durable high-water mark.
	MsgAck byte = 0x04
	// MsgError reports a failure; the payload is a human-readable reason.
	MsgError byte = 0x05
	// MsgPing keeps an idle connection alive and proves liveness.
	MsgPing byte = 0x06
	// MsgPong answers MsgPing.
	MsgPong byte = 0x07
	// MsgEpochRequest asks the leader where a leader epoch ended. A follower
	// sends one per partition before its first fetch of a session.
	MsgEpochRequest byte = 0x08
	// MsgEpochResponse carries the end sequence of the queried epoch.
	MsgEpochResponse byte = 0x09
)

// MaxFramePayload caps one protocol frame. A records batch is bounded by the
// leader's batch settings, comfortably below this; the cap exists so a
// corrupted length cannot make the peer allocate wildly.
const MaxFramePayload = 16 << 20

var (
	// ErrProtocol means the peer sent something malformed.
	ErrProtocol = errors.New("replication: protocol error")
	// ErrFrameTooLarge means a frame exceeded MaxFramePayload.
	ErrFrameTooLarge = errors.New("replication: frame too large")
)

// writeFrame writes one protocol frame.
func writeFrame(w io.Writer, typ byte, payload []byte) error {
	if len(payload) > MaxFramePayload {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(payload))
	}
	var header [5]byte
	header[0] = typ
	binary.LittleEndian.PutUint32(header[1:5], uint32(len(payload)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one protocol frame.
func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.LittleEndian.Uint32(header[1:5])
	if length > MaxFramePayload {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}
	if length == 0 {
		return header[0], nil, nil
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// fetchRequest asks the leader to stream a partition.
//
// Encoded as [fromSeq:8][partition:4][partitionCount:4][topicLen:2][topic]
// rather than JSON: a follower re-issues this on every reconnect and after
// every gap, and the fixed layout parses with no allocation.
type fetchRequest struct {
	Topic string
	// PartitionCount lets a follower create the topic locally with the same
	// partition count as the leader. Without it the follower would guess, and a
	// wrong guess remaps every key.
	PartitionCount int32
	Partition      int32
	FromSeq        uint64
}

const fetchHeaderSize = 18 // fromSeq + partition + partitionCount + topicLen

func (f fetchRequest) encode() []byte {
	buf := make([]byte, fetchHeaderSize+len(f.Topic))
	binary.LittleEndian.PutUint64(buf[0:8], f.FromSeq)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(f.Partition))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(f.PartitionCount))
	binary.LittleEndian.PutUint16(buf[16:18], uint16(len(f.Topic)))
	copy(buf[fetchHeaderSize:], f.Topic)
	return buf
}

func decodeFetch(b []byte) (fetchRequest, error) {
	if len(b) < fetchHeaderSize {
		return fetchRequest{}, fmt.Errorf("%w: short fetch", ErrProtocol)
	}
	topicLen := int(binary.LittleEndian.Uint16(b[16:18]))
	if fetchHeaderSize+topicLen != len(b) {
		return fetchRequest{}, fmt.Errorf("%w: fetch topic length mismatch", ErrProtocol)
	}
	return fetchRequest{
		FromSeq:        binary.LittleEndian.Uint64(b[0:8]),
		Partition:      int32(binary.LittleEndian.Uint32(b[8:12])),
		PartitionCount: int32(binary.LittleEndian.Uint32(b[12:16])),
		Topic:          string(b[fetchHeaderSize:]),
	}, nil
}

// ackMessage reports how far a follower has durably applied a partition.
//
// Layout: [seq:8][partition:4][topicLen:2][topic]
type ackMessage struct {
	Topic     string
	Partition int32
	Seq       uint64
}

const ackHeaderSize = 14

func (a ackMessage) encode() []byte {
	buf := make([]byte, ackHeaderSize+len(a.Topic))
	binary.LittleEndian.PutUint64(buf[0:8], a.Seq)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(a.Partition))
	binary.LittleEndian.PutUint16(buf[12:14], uint16(len(a.Topic)))
	copy(buf[ackHeaderSize:], a.Topic)
	return buf
}

func decodeAck(b []byte) (ackMessage, error) {
	if len(b) < ackHeaderSize {
		return ackMessage{}, fmt.Errorf("%w: short ack", ErrProtocol)
	}
	topicLen := int(binary.LittleEndian.Uint16(b[12:14]))
	if ackHeaderSize+topicLen != len(b) {
		return ackMessage{}, fmt.Errorf("%w: ack topic length mismatch", ErrProtocol)
	}
	return ackMessage{
		Seq:       binary.LittleEndian.Uint64(b[0:8]),
		Partition: int32(binary.LittleEndian.Uint32(b[8:12])),
		Topic:     string(b[ackHeaderSize:]),
	}, nil
}

// epochRequest asks "where did this leader epoch end?" for one partition.
//
// Layout: [epoch:4][partition:4][topicLen:2][topic]
type epochRequest struct {
	Topic     string
	Partition int32
	Epoch     uint32
}

const epochRequestSize = 10

func (e epochRequest) encode() []byte {
	buf := make([]byte, epochRequestSize+len(e.Topic))
	binary.LittleEndian.PutUint32(buf[0:4], e.Epoch)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(e.Partition))
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(e.Topic)))
	copy(buf[epochRequestSize:], e.Topic)
	return buf
}

func decodeEpochRequest(b []byte) (epochRequest, error) {
	if len(b) < epochRequestSize {
		return epochRequest{}, fmt.Errorf("%w: short epoch request", ErrProtocol)
	}
	topicLen := int(binary.LittleEndian.Uint16(b[8:10]))
	if epochRequestSize+topicLen != len(b) {
		return epochRequest{}, fmt.Errorf("%w: epoch request topic length mismatch", ErrProtocol)
	}
	return epochRequest{
		Epoch:     binary.LittleEndian.Uint32(b[0:4]),
		Partition: int32(binary.LittleEndian.Uint32(b[4:8])),
		Topic:     string(b[epochRequestSize:]),
	}, nil
}

// epochResponse answers an epochRequest.
//
// Epoch is the term that actually covers the answer, which may be older than
// the one asked about. Zero means the leader cannot place the query in its
// history — the follower must then re-fetch from the leader's first sequence
// rather than truncate on an assumption.
//
// Layout: [epoch:4][endSeq:8][partition:4][topicLen:2][topic]
type epochResponse struct {
	Topic     string
	Partition int32
	Epoch     uint32
	EndSeq    uint64
}

const epochResponseSize = 18

func (e epochResponse) encode() []byte {
	buf := make([]byte, epochResponseSize+len(e.Topic))
	binary.LittleEndian.PutUint32(buf[0:4], e.Epoch)
	binary.LittleEndian.PutUint64(buf[4:12], e.EndSeq)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(e.Partition))
	binary.LittleEndian.PutUint16(buf[16:18], uint16(len(e.Topic)))
	copy(buf[epochResponseSize:], e.Topic)
	return buf
}

func decodeEpochResponse(b []byte) (epochResponse, error) {
	if len(b) < epochResponseSize {
		return epochResponse{}, fmt.Errorf("%w: short epoch response", ErrProtocol)
	}
	topicLen := int(binary.LittleEndian.Uint16(b[16:18]))
	if epochResponseSize+topicLen != len(b) {
		return epochResponse{}, fmt.Errorf("%w: epoch response topic length mismatch", ErrProtocol)
	}
	return epochResponse{
		Epoch:     binary.LittleEndian.Uint32(b[0:4]),
		EndSeq:    binary.LittleEndian.Uint64(b[4:12]),
		Partition: int32(binary.LittleEndian.Uint32(b[12:16])),
		Topic:     string(b[epochResponseSize:]),
	}, nil
}

// recordsHeader precedes a batch of records in a MsgRecords frame.
//
// Layout: [partition:4][count:4][topicLen:2][topic][record frames...]
type recordsHeader struct {
	Topic     string
	Partition int32
	Count     uint32
}

const recordsHeaderSize = 10

func encodeRecordsHeader(h recordsHeader) []byte {
	buf := make([]byte, recordsHeaderSize+len(h.Topic))
	binary.LittleEndian.PutUint32(buf[0:4], uint32(h.Partition))
	binary.LittleEndian.PutUint32(buf[4:8], h.Count)
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(h.Topic)))
	copy(buf[recordsHeaderSize:], h.Topic)
	return buf
}

// decodeRecordsHeader parses the header and returns the offset at which the
// record frames begin.
func decodeRecordsHeader(b []byte) (recordsHeader, int, error) {
	if len(b) < recordsHeaderSize {
		return recordsHeader{}, 0, fmt.Errorf("%w: short records header", ErrProtocol)
	}
	topicLen := int(binary.LittleEndian.Uint16(b[8:10]))
	if len(b) < recordsHeaderSize+topicLen {
		return recordsHeader{}, 0, fmt.Errorf("%w: records topic truncated", ErrProtocol)
	}
	return recordsHeader{
		Partition: int32(binary.LittleEndian.Uint32(b[0:4])),
		Count:     binary.LittleEndian.Uint32(b[4:8]),
		Topic:     string(b[recordsHeaderSize : recordsHeaderSize+topicLen]),
	}, recordsHeaderSize + topicLen, nil
}
