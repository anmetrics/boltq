package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Command bytes for TCP protocol.
const (
	CmdPublish      byte = 0x01
	CmdPublishTopic byte = 0x02
	CmdConsume      byte = 0x03
	CmdAck          byte = 0x04
	CmdNack         byte = 0x05
	CmdPing         byte = 0x06
	CmdStats        byte = 0x07
	CmdAuth          byte = 0x08
	CmdClusterJoin   byte = 0x10
	CmdClusterLeave  byte = 0x11
	CmdClusterStatus byte = 0x12
)

// Response status bytes.
const (
	StatusOK        byte = 0x00
	StatusError     byte = 0x01
	StatusEmpty     byte = 0x02
	StatusNotLeader byte = 0x03
)

// MaxFrameSize is the maximum allowed payload size (4MB).
const MaxFrameSize = 4 << 20

// Frame represents a TCP protocol frame.
type Frame struct {
	Command byte // request: command byte; response: status byte
	Payload []byte
}

// WriteFrame writes a frame to the writer.
// Wire format: [command/status:1B][length:4B LE][payload:N bytes]
func WriteFrame(w io.Writer, f Frame) error {
	header := make([]byte, 5)
	header[0] = f.Command
	binary.LittleEndian.PutUint32(header[1:5], uint32(len(f.Payload)))
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("write payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads a frame from the reader.
func ReadFrame(r io.Reader) (Frame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return Frame{}, err
	}

	cmd := header[0]
	length := binary.LittleEndian.Uint32(header[1:5])

	if length > MaxFrameSize {
		return Frame{}, fmt.Errorf("frame too large: %d bytes (max %d)", length, MaxFrameSize)
	}

	var payload []byte
	if length > 0 {
		payload = make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, fmt.Errorf("read payload: %w", err)
		}
	}

	return Frame{Command: cmd, Payload: payload}, nil
}
