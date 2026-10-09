package jobs

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateNumbersJobsWithoutReuse(t *testing.T) {
	root := t.TempDir()
	var ids []string
	for range 3 {
		m, err := Create(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Save(); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "1,2,3" {
		t.Fatalf("ids = %v", ids)
	}
	// Removing the newest must not hand its number out again.
	os.RemoveAll(filepath.Join(root, "3"))
	m, _ := Create(root)
	if m.ID != "4" {
		t.Errorf("next id = %s, want 4", m.ID)
	}
	if got := List(root); len(got) != 2 || got[0].ID != "2" {
		t.Errorf("List = %v", got)
	}
}

// finished saves a job that ended ago.
func finished(t *testing.T, root string, ago time.Duration, code int) *Meta {
	t.Helper()
	m, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(-ago)
	m.Started, m.Ended, m.ExitCode, m.HelperPID = end.Add(-time.Minute), &end, code, 1
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStates(t *testing.T) {
	root := t.TempDir()
	done := finished(t, root, time.Hour, 2)
	if done.State() != StateExited || done.Running() || done.Outcome() != "exit 2" {
		t.Errorf("finished: %s %s", done.State(), done.Outcome())
	}
	if !strings.HasPrefix(done.Summary(), "failed (exit 2) after 1m0s") {
		t.Errorf("summary = %q", done.Summary())
	}

	running, _ := Create(root)
	running.HelperPID = os.Getpid()
	running.Save()
	running, _ = running.Reload()
	if running.State() != StateRunning {
		t.Errorf("running state = %s", running.State())
	}

	starting, _ := Create(root)
	starting.Save()
	starting, _ = starting.Reload()
	if starting.State() != StateStarting {
		t.Errorf("starting state = %s", starting.State())
	}

	lost, _ := Create(root)
	lost.HelperPID = deadPID(t)
	lost.Save()
	lost, _ = lost.Reload()
	if lost.State() != StateLost || lost.Running() {
		t.Errorf("lost state = %s", lost.State())
	}
}

func deadPID(t *testing.T) int {
	t.Helper()
	// A pid well past the usual range is all but certainly free.
	for pid := 4_000_000; pid < 4_000_100; pid++ {
		if !alive(pid) {
			return pid
		}
	}
	t.Skip("no free pid found")
	return 0
}

func TestCleanKeepsRunningAndHonoursLimits(t *testing.T) {
	root := t.TempDir()
	old := finished(t, root, 10*24*time.Hour, 0)
	recent := []*Meta{
		finished(t, root, 3*time.Hour, 0),
		finished(t, root, 2*time.Hour, 0),
		finished(t, root, time.Hour, 0),
	}
	run, _ := Create(root)
	run.HelperPID = os.Getpid()
	run.Save()

	if n := Clean(root, 7*24*time.Hour, 0); n != 1 {
		t.Fatalf("age clean removed %d, want 1", n)
	}
	if _, err := Load(old.JobDir()); err == nil {
		t.Error("old job kept")
	}
	if n := Clean(root, 0, 2); n != 1 {
		t.Fatalf("count clean removed %d, want 1", n)
	}
	if _, err := Load(recent[0].JobDir()); err == nil {
		t.Error("oldest of the recent jobs kept")
	}
	if n := Clean(root, time.Nanosecond, 0); n != 2 {
		t.Fatalf("removed %d, want the 2 finished", n)
	}
	if got := List(root); len(got) != 1 || got[0].ID != run.ID {
		t.Errorf("left = %v, want only the running job", got)
	}
	if err := Remove(got0(root)); err == nil {
		t.Error("Remove deleted a running job")
	}
}

func got0(root string) *Meta { return List(root)[0] }

func TestSocketPathFallsBackWhenTooLong(t *testing.T) {
	short := socketPath("/tmp/j/1")
	if short != "/tmp/j/1/sock" {
		t.Errorf("short = %s", short)
	}
	long := socketPath("/" + strings.Repeat("x", 120) + "/1")
	if len(long) >= 104 || !strings.HasPrefix(filepath.Base(long), "mm-") {
		t.Errorf("long = %s", long)
	}
	if long != socketPath("/"+strings.Repeat("x", 120)+"/1") {
		t.Error("fallback isn't stable")
	}
}

func TestFrames(t *testing.T) {
	var b bytes.Buffer
	writeFrame(&b, frameResize, sizePayload(40, 120))
	writeFrame(&b, frameInput, []byte("hi"))
	typ, p, err := readFrame(&b)
	if rows, cols, ok := parseSize(p); err != nil || typ != frameResize || !ok || rows != 40 || cols != 120 {
		t.Errorf("resize frame: %c %v %v", typ, p, err)
	}
	typ, p, _ = readFrame(&b)
	if typ != frameInput || string(p) != "hi" {
		t.Errorf("input frame: %c %q", typ, p)
	}
	if _, _, err := readFrame(&b); err == nil {
		t.Error("read past the end")
	}

	// A Conn skips frames it doesn't know and decodes exits.
	client, server := net.Pipe()
	go func() {
		writeFrame(server, '?', nil)
		writeFrame(server, frameOutput, []byte("out"))
		writeFrame(server, frameExit, []byte(`{"code":3,"outcome":"exit 3"}`))
	}()
	c := &Conn{c: client}
	out, exit, err := c.Next()
	if string(out) != "out" || exit != nil || err != nil {
		t.Errorf("Next = %q %v %v", out, exit, err)
	}
	if _, exit, _ = c.Next(); exit == nil || exit.Code != 3 {
		t.Errorf("exit = %+v", exit)
	}
}

func TestReplayStartsAtALine(t *testing.T) {
	h := &hub{recent: []byte("\x1b[1mpartial\nwhole line\n"), trimmed: true}
	if got := string(h.replay()); got != "whole line\n" {
		t.Errorf("replay = %q", got)
	}
	h.trimmed = false
	if got := string(h.replay()); !strings.HasPrefix(got, "\x1b[1m") {
		t.Errorf("untrimmed replay = %q", got)
	}
}

func TestHelperServesClients(t *testing.T) {
	root := t.TempDir()
	m, err := Create(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Macro, m.Path, m.Dir, m.Rows, m.Cols, m.Notify = "personal/t", "/bin/sh", root, 10, 80, "off"
	m.Argv = []string{"sh", "-c", `echo ready; read x; echo "got $x"; stty size; exit 5`}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- Helper(m.JobDir()) }()

	var c *Conn
	for deadline := time.Now().Add(5 * time.Second); ; {
		if c, err = Dial(m); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer c.Close()
	c.Hello(30, 100)
	// A second client watching at the same time sees the same output.
	c2, err := Dial(m)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.Hello(30, 100)

	var out bytes.Buffer
	sent := false
	for {
		b, exit, err := c.Next()
		if err != nil {
			t.Fatalf("Next: %v (output %q)", err, out.String())
		}
		out.Write(b)
		if !sent && strings.Contains(out.String(), "ready") {
			c.Input([]byte("bob\n"))
			sent = true
		}
		if exit != nil {
			if exit.Code != 5 || exit.Outcome != "exit 5" {
				t.Errorf("exit = %+v", exit)
			}
			break
		}
	}
	if s := out.String(); !strings.Contains(s, "got bob") || !strings.Contains(s, "30 100") {
		t.Errorf("output = %q", s)
	}
	var out2 bytes.Buffer
	for {
		b, exit, err := c2.Next()
		if err != nil || exit != nil {
			break
		}
		out2.Write(b)
	}
	if !strings.Contains(out2.String(), "got bob") {
		t.Errorf("second client saw %q", out2.String())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m, _ = m.Reload()
	if m.State() != StateExited || m.ExitCode != 5 {
		t.Errorf("meta: %s %d", m.State(), m.ExitCode)
	}
	if log, _ := os.ReadFile(m.LogPath()); !strings.Contains(string(log), "got bob") {
		t.Errorf("log = %q", log)
	}
	if _, err := os.Stat(m.SocketPath()); !os.IsNotExist(err) {
		t.Error("socket left behind")
	}
}
