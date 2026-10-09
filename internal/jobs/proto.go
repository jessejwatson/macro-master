package jobs

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// Frames on the helper's socket are a type byte, a big-endian uint32
// length and the payload.
const (
	// Client to helper.
	frameHello  = 'h' // rows, cols as uint16s; the helper replays recent output
	frameInput  = 'i' // bytes for the job's terminal
	frameResize = 'z' // rows, cols as uint16s
	frameStop   = 'k' // 'T' terminates the job, 'K' kills it
	frameName   = 'n' // the job's new name

	// Helper to client.
	frameOutput = 'o'
	frameExit   = 'x' // JSON Exit
)

const maxFrame = 1 << 20

// Exit is how a job ended, as sent to attached clients.
type Exit struct {
	Code    int    `json:"code"`
	Outcome string `json:"outcome"`
	Bell    bool   `json:"bell,omitempty"`
}

func writeFrame(w io.Writer, typ byte, payload []byte) error {
	buf := make([]byte, 5+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

func sizePayload(rows, cols int) []byte {
	var b [4]byte
	binary.BigEndian.PutUint16(b[0:2], uint16(rows))
	binary.BigEndian.PutUint16(b[2:4], uint16(cols))
	return b[:]
}

func parseSize(p []byte) (rows, cols int, ok bool) {
	if len(p) != 4 {
		return 0, 0, false
	}
	return int(binary.BigEndian.Uint16(p[0:2])), int(binary.BigEndian.Uint16(p[2:4])), true
}

// Conn is a client's connection to a running job.
type Conn struct {
	c net.Conn
}

// Dial connects to the job's helper.
func Dial(m *Meta) (*Conn, error) {
	c, err := net.DialTimeout("unix", m.SocketPath(), 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c}, nil
}

// Hello asks for recent output at the given terminal size. It must be the
// first frame sent.
func (c *Conn) Hello(rows, cols int) error {
	return writeFrame(c.c, frameHello, sizePayload(rows, cols))
}

// Input sends keys to the job.
func (c *Conn) Input(b []byte) error { return writeFrame(c.c, frameInput, b) }

// Resize changes the job's terminal size.
func (c *Conn) Resize(rows, cols int) error {
	return writeFrame(c.c, frameResize, sizePayload(rows, cols))
}

// Terminate asks the helper to stop the job: SIGTERM, then SIGKILL if it's
// still going a few seconds later. Kill sends SIGKILL straight away.
func (c *Conn) Terminate() error { return writeFrame(c.c, frameStop, []byte{'T'}) }
func (c *Conn) Kill() error      { return writeFrame(c.c, frameStop, []byte{'K'}) }

// SetName names the job.
func (c *Conn) SetName(name string) error { return writeFrame(c.c, frameName, []byte(name)) }

// Next reads the next message from the helper: output, or the job's end.
func (c *Conn) Next() (output []byte, exit *Exit, err error) {
	for {
		typ, p, err := readFrame(c.c)
		if err != nil {
			return nil, nil, err
		}
		switch typ {
		case frameOutput:
			return p, nil, nil
		case frameExit:
			var e Exit
			if err := json.Unmarshal(p, &e); err != nil {
				return nil, nil, err
			}
			return nil, &e, nil
		}
	}
}

// Close disconnects; the job keeps running.
func (c *Conn) Close() error { return c.c.Close() }
