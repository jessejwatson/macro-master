package jobs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	// replayMax is how much recent output a newly attached client gets.
	replayMax = 256 << 10
	// logMax is when output.log is rotated to output.log.1.
	logMax = 8 << 20
)

// Helper runs the job in dir: it starts the macro on a pseudo-terminal,
// logs its output, serves clients on the socket and records how it ended.
// MM_MACRO and MM_LIBRARY for the macro come from the helper's own
// environment.
func Helper(dir string) error {
	m, err := Load(dir)
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	fail := func(err error) error {
		now := time.Now().UTC()
		m.Error, m.Ended, m.ExitCode = err.Error(), &now, 127
		m.Save()
		return err
	}

	sock := m.SocketPath()
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fail(fmt.Errorf("can't listen on %s: %w", sock, err))
	}
	defer os.Remove(sock)
	defer ln.Close()

	logf, err := os.OpenFile(m.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fail(err)
	}
	defer logf.Close()

	cmd := exec.Command(m.Path, m.Argv[1:]...)
	cmd.Args[0] = m.Argv[0]
	cmd.Dir = m.Dir
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(max(m.Rows, 2)), Cols: uint16(max(m.Cols, 20))})
	if err != nil {
		return fail(fmt.Errorf("can't start %s: %w", m.Macro, err))
	}
	defer ptmx.Close()
	m.HelperPID, m.PID = os.Getpid(), cmd.Process.Pid
	if err := m.Save(); err != nil {
		cmd.Process.Kill()
		return err
	}

	h := &hub{meta: m, ptmx: ptmx, log: logf, clients: map[*client]bool{}}
	if fi, err := logf.Stat(); err == nil {
		h.logSize = fi.Size()
	}
	go h.serve(ln)
	pumped := make(chan struct{})
	go func() {
		h.pump()
		close(pumped)
	}()

	waitErr := cmd.Wait()
	// Let the last output through; a grandchild holding the terminal open
	// mustn't keep the job alive.
	select {
	case <-pumped:
	case <-time.After(500 * time.Millisecond):
	}

	now := time.Now().UTC()
	m.Ended = &now
	m.ExitCode, m.Signal = exitInfo(waitErr)
	if err := m.Save(); err != nil {
		return err
	}
	if m.Script != "" {
		os.Remove(m.Script)
	}

	notify := m.Notify
	attached := h.finish(Exit{
		Code:    m.ExitCode,
		Outcome: m.Outcome(),
		Bell:    notify == "bell" || notify == "both",
	})
	if attached == 0 && (notify == "desktop" || notify == "both") {
		Notify(m)
	}
	return nil
}

// exitInfo turns cmd.Wait's result into an exit code, using 128+n for a
// job killed by signal n as shells do.
func exitInfo(err error) (code, sig int) {
	if err == nil {
		return 0, 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 127, 0
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), int(ws.Signal())
	}
	return ee.ExitCode(), 0
}

// hub fans the job's output out to the log, the replay buffer and every
// attached client.
type hub struct {
	meta    *Meta
	ptmx    *os.File
	log     *os.File
	logSize int64

	mu      sync.Mutex
	recent  []byte
	trimmed bool // recent has lost its start
	clients map[*client]bool
	ended   *Exit
}

type client struct {
	conn   net.Conn
	out    chan []byte // whole frames
	mu     sync.Mutex
	closed bool
}

// send queues a frame, dropping a client too slow to keep up rather than
// stalling the job.
func (c *client) send(frame []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.out <- frame:
		return true
	default:
		c.closed = true
		close(c.out)
		return false
	}
}

// close lets the writer finish what's queued, then hang up.
func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.out)
	}
}

func (c *client) writer() {
	for f := range c.out {
		if _, err := c.conn.Write(f); err != nil {
			break
		}
	}
	c.conn.Close()
}

func frame(typ byte, payload []byte) []byte {
	var b bytes.Buffer
	writeFrame(&b, typ, payload)
	return b.Bytes()
}

func (h *hub) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := h.ptmx.Read(buf)
		if n > 0 {
			h.output(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

func (h *hub) output(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recent = append(h.recent, data...)
	if over := len(h.recent) - replayMax; over > 0 {
		h.recent = append(h.recent[:0:0], h.recent[over:]...)
		h.trimmed = true
	}
	h.writeLog(data)
	f := frame(frameOutput, data)
	for c := range h.clients {
		if !c.send(f) {
			delete(h.clients, c)
		}
	}
}

func (h *hub) writeLog(data []byte) {
	if h.logSize+int64(len(data)) > logMax {
		p := h.log.Name()
		if os.Rename(p, p+".1") == nil {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600); err == nil {
				h.log.Close()
				h.log, h.logSize = f, 0
			}
		}
	}
	n, _ := h.log.Write(data)
	h.logSize += int64(n)
}

// replay is the recent output, starting at a line boundary when the start
// was cut off so it doesn't begin halfway through an escape sequence.
func (h *hub) replay() []byte {
	r := h.recent
	if h.trimmed {
		if i := bytes.IndexByte(r, '\n'); i >= 0 {
			r = r[i+1:]
		}
	}
	return append([]byte(nil), r...)
}

func (h *hub) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handle(conn)
	}
}

func (h *hub) handle(conn net.Conn) {
	c := &client{conn: conn, out: make(chan []byte, 1024)}
	go c.writer()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		c.close()
	}()
	for {
		typ, p, err := readFrame(conn)
		if err != nil {
			return
		}
		switch typ {
		case frameHello:
			if rows, cols, ok := parseSize(p); ok {
				h.resize(rows, cols)
			}
			h.mu.Lock()
			if r := h.replay(); len(r) > 0 {
				c.send(frame(frameOutput, r))
			}
			if h.ended != nil {
				b, _ := json.Marshal(h.ended)
				c.send(frame(frameExit, b))
			} else {
				h.clients[c] = true
			}
			h.mu.Unlock()
		case frameInput:
			h.ptmx.Write(p)
		case frameResize:
			if rows, cols, ok := parseSize(p); ok {
				h.resize(rows, cols)
			}
		case frameStop:
			h.stop(len(p) == 1 && p[0] == 'K')
		}
	}
}

func (h *hub) resize(rows, cols int) {
	if rows < 1 || cols < 1 {
		return
	}
	pty.Setsize(h.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// stop signals the job's process group and whatever group is in the
// foreground of its terminal.
func (h *hub) stop(kill bool) {
	groups := []int{h.meta.PID}
	if fg, err := unix.IoctlGetInt(int(h.ptmx.Fd()), unix.TIOCGPGRP); err == nil && fg > 0 && fg != h.meta.PID {
		groups = append(groups, fg)
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	for _, g := range groups {
		syscall.Kill(-g, sig)
	}
	if !kill {
		time.AfterFunc(3*time.Second, func() {
			for _, g := range groups {
				syscall.Kill(-g, syscall.SIGKILL)
			}
		})
	}
}

// finish tells attached clients the job has ended, waits briefly for them
// to receive it, and returns how many there were.
func (h *hub) finish(e Exit) int {
	b, _ := json.Marshal(e)
	f := frame(frameExit, b)
	h.mu.Lock()
	h.ended = &e
	clients := h.clients
	h.clients = map[*client]bool{}
	h.mu.Unlock()
	for c := range clients {
		c.send(f)
		c.close()
	}
	deadline := time.Now().Add(time.Second)
	for c := range clients {
		for len(c.out) > 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	return len(clients)
}
