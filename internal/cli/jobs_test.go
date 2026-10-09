package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"macro-master/internal/jobs"
)

// newJobApp is a test app whose detached jobs run for real: the helper is
// this test binary acting as mm. Notifications are off.
func newJobApp(t *testing.T) *testApp {
	t.Helper()
	ta := newApp(t)
	os.WriteFile(filepath.Join(ta.Home, "config.json"),
		[]byte(`{"default_library":"personal","jobs":{"notify":"off"}}`), 0o644)
	ta.StartJob = func(dir string, env ...string) error {
		cmd := exec.Command(os.Args[0], "__job", dir)
		cmd.Env = append(os.Environ(), append(env, "MM_TEST_MAIN=1", "MM_HOME="+ta.Home)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
	t.Cleanup(func() {
		for _, m := range jobs.List(filepath.Join(ta.Home, "jobs")) {
			if m.Running() && m.PID != 0 {
				syscall.Kill(-m.PID, syscall.SIGKILL)
			}
		}
	})
	return ta
}

func (ta *testApp) job(t *testing.T, id string) *jobs.Meta {
	t.Helper()
	m, err := jobs.Get(filepath.Join(ta.Home, "jobs"), id)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// waitEnd waits for job id to finish.
func (ta *testApp) waitEnd(t *testing.T, id string) *jobs.Meta {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := ta.job(t, id); !m.Running() {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s still running", id)
	return nil
}

func TestDetachedRunListAndFinishedAttach(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/greet", "echo \"hi {{who}} $1 $MM_MACRO\"\nexit 4\n")

	if code := ta.run(nil, "-d", "greet", "who=bob", "extra"); code != 0 {
		t.Fatalf("-d: %d %s", code, ta.err)
	}
	if !strings.Contains(ta.err.String(), "Started job 1: personal/greet") {
		t.Errorf("start message = %q", ta.err)
	}
	m := ta.waitEnd(t, "1")
	if m.ExitCode != 4 || m.Outcome() != "exit 4" {
		t.Errorf("job ended %s", m.Outcome())
	}
	if _, err := os.Stat(m.Script); !os.IsNotExist(err) {
		t.Error("script copy left behind")
	}
	if _, ok := ta.store.State.LastRun["personal/greet"]; !ok {
		t.Error("detached run not recorded")
	}

	if code := ta.run(nil, "jobs"); code != 0 || !regexp.MustCompile(`1\s+✗ exit 4\s+personal/greet`).MatchString(ta.out.String()) {
		t.Errorf("jobs: %d %q", code, ta.out)
	}
	// Attaching to a finished job shows its output and returns its status.
	if code := ta.run(nil, "attach", "greet"); code != 4 {
		t.Errorf("attach exit = %d", code)
	}
	if got := ta.out.String(); !strings.Contains(got, "hi bob extra greet") {
		t.Errorf("attach output = %q", got)
	}
	if !strings.Contains(ta.err.String(), "Job 1 failed (exit 4)") {
		t.Errorf("attach summary = %q", ta.err)
	}

	if code := ta.run(nil, "run", "--detach", "greet", "who=amy"); code != 0 {
		t.Fatalf("run --detach: %s", ta.err)
	}
	ta.waitEnd(t, "2")
	if code := ta.run(nil, "jobs", "clear"); code != 0 || !strings.Contains(ta.err.String(), "Cleared 2 finished jobs") {
		t.Errorf("clear: %q", ta.err)
	}
}

func TestDetachRefusalsAndErrors(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/cd", "# mode: source\ncd /tmp\n")
	ta.save(t, "personal/ask", "echo {{x}}\n")
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-d", "cd"}, "can't run detached"},
		{[]string{"-d"}, "-d needs a macro name"},
		{[]string{"run", "-d"}, "mm run needs a macro name"},
		{[]string{"-d", "ask"}, "{{x}} needs a value"},
		{[]string{"attach"}, "no jobs are running"},
		{[]string{"attach", "9"}, `no job "9"`},
		{[]string{"kill", "nope"}, `no job "nope"`},
		{[]string{"jobs", "x"}, "usage: mm jobs [clear]"},
	}
	for _, c := range cases {
		if code := ta.run(nil, c.args...); code == 0 || !strings.Contains(ta.err.String(), c.want) {
			t.Errorf("%v: exit %d, err %q, want %q", c.args, code, ta.err, c.want)
		}
	}
	if code := ta.run(nil, "jobs"); code != 0 || !strings.Contains(ta.err.String(), "No jobs") {
		t.Errorf("empty jobs: %q", ta.err)
	}
}

func TestKillAndPickTheOnlyRunningJob(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/long", "echo up\nsleep 30\n")
	ta.save(t, "personal/stubborn", "trap '' TERM INT\necho up\nwhile :; do sleep 0.1; done\n")
	ta.run(nil, "-d", "long")
	ta.run(nil, "-d", "stubborn")

	if code := ta.run(nil, "kill"); code == 0 || !strings.Contains(ta.err.String(), "2 jobs are running") {
		t.Errorf("ambiguous kill: %q", ta.err)
	}
	if code := ta.run(nil, "kill", "long"); code != 0 {
		t.Fatalf("kill: %s", ta.err)
	}
	if m := ta.job(t, "1"); m.Running() || m.Outcome() != "terminated" {
		t.Errorf("long ended %s", m.Outcome())
	}
	// Ignoring SIGTERM only buys a few seconds.
	if code := ta.run(nil, "kill"); code != 0 {
		t.Fatalf("kill stubborn: %s", ta.err)
	}
	if m := ta.job(t, "2"); m.Outcome() != "killed" {
		t.Errorf("stubborn ended %s", m.Outcome())
	}
	if code := ta.run(nil, "kill", "2"); code == 0 || !strings.Contains(ta.err.String(), "already ended") {
		t.Errorf("kill ended job: %q", ta.err)
	}
}

func TestCleanupFollowsConfig(t *testing.T) {
	ta := newJobApp(t)
	os.WriteFile(filepath.Join(ta.Home, "config.json"),
		[]byte(`{"default_library":"personal","jobs":{"notify":"off","keep_max":1}}`), 0o644)
	ta.save(t, "personal/quick", "true\n")
	for _, id := range []string{"1", "2", "3"} {
		ta.run(nil, "-d", "quick")
		ta.waitEnd(t, id)
	}
	ta.run(nil, "jobs")
	ids := regexp.MustCompile(`(?m)^(\d+)\s`).FindAllStringSubmatch(ta.out.String(), -1)
	if len(ids) != 1 || ids[0][1] != "3" {
		t.Errorf("kept %v, want just job 3:\n%s", ids, ta.out)
	}
}

// ptyTerm runs mm under a pseudo-terminal and collects what it draws.
type ptyTerm struct {
	t    *testing.T
	f    *os.File
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func startPty(t *testing.T, home string, args ...string) *ptyTerm {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "MM_TEST_MAIN=1", "MM_HOME="+home, "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 12, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	p := &ptyTerm{t: t, f: f, cmd: cmd, done: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			p.mu.Lock()
			p.out.Write(buf[:n])
			p.mu.Unlock()
			if err != nil {
				close(p.done)
				return
			}
		}
	}()
	t.Cleanup(func() { cmd.Process.Kill(); f.Close() })
	return p
}

func (p *ptyTerm) screen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

// waitFor waits until the output contains s.
func (p *ptyTerm) waitFor(s string) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.screen(), s) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.t.Fatalf("never saw %q in:\n%q", s, p.screen())
}

func (p *ptyTerm) send(s string) {
	p.f.Write([]byte(s))
	time.Sleep(150 * time.Millisecond)
}

// exit waits for mm to finish and returns its exit code.
func (p *ptyTerm) exit() int {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		p.t.Fatalf("mm didn't exit:\n%q", p.screen())
	}
	p.cmd.Wait()
	return p.cmd.ProcessState.ExitCode()
}

func TestAttachWatchTypeDetachAndStop(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/ask", "echo ready\nread -r -p 'name? ' n\necho \"hello $n\"\nsleep 30\n")
	if code := ta.run(nil, "-d", "ask"); code != 0 {
		t.Fatalf("-d: %s", ta.err)
	}

	p := startPty(t, ta.Home, "attach", "1")
	p.waitFor("WATCHING")
	p.waitFor("name? ")
	// Watching: other keys are ignored, not typed into the job.
	p.send("xyz")
	p.send("i")
	p.waitFor("TYPING")
	p.send("bob\r")
	p.waitFor("hello bob")
	p.send("\x1c") // ctrl-\ back to watching
	p.send("d")
	if code := p.exit(); code != 0 {
		t.Errorf("detach exit = %d", code)
	}
	if s := p.screen(); !strings.Contains(s, "Detached from job 1") || !strings.Contains(s, "\x1b[r") {
		t.Errorf("detach didn't restore the terminal:\n%q", s)
	}
	if strings.Contains(string(readLog(t, ta.job(t, "1"))), "xyz") {
		t.Error("keys typed while watching reached the job")
	}

	// Reattaching replays what came before; ctrl+c stops the job.
	p = startPty(t, ta.Home, "attach")
	p.waitFor("hello bob")
	p.send("\x03")
	if code := p.exit(); code != 130 {
		t.Errorf("interrupted attach exit = %d, want 130", code)
	}
	if !strings.Contains(p.screen(), "Job 1 interrupted") {
		t.Errorf("end line missing:\n%q", p.screen())
	}
}

func TestAttachCtrlCTwiceKills(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/stubborn", "trap 'echo ignoring' INT\necho up\nwhile :; do sleep 0.1; done\n")
	ta.run(nil, "-d", "stubborn")
	p := startPty(t, ta.Home, "attach", "1")
	p.waitFor("up")
	p.send("\x03")
	p.waitFor("ctrl+c again to kill")
	p.waitFor("ignoring")
	p.send("\x03")
	if code := p.exit(); code != 128+9 {
		t.Errorf("exit = %d, want 137", code)
	}
	if m := ta.job(t, "1"); m.Outcome() != "killed" {
		t.Errorf("outcome = %s", m.Outcome())
	}
}

func readLog(t *testing.T, m *jobs.Meta) []byte {
	b, err := os.ReadFile(m.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPreviewJob(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/bar", "printf '\\033[2J\\033[31mred\\033[0m\\n10%%\\r50%%\\r100%%\\n'\n")
	ta.run(nil, "-d", "bar")
	ta.waitEnd(t, "1")
	if code := ta.run(nil, "__preview", "--plain", "job:1"); code != 0 {
		t.Fatalf("preview: %s", ta.err)
	}
	got := ta.out.String()
	if !strings.Contains(got, "job 1 personal/bar") || !strings.Contains(got, "\x1b[31mred\x1b[0m\n100%") || strings.Contains(got, "[2J") {
		t.Errorf("preview = %q", got)
	}
}

func TestScreenSequences(t *testing.T) {
	cases := []struct {
		in      string
		touched bool
		alt     int
	}{
		{"plain text\r\n", false, 0},
		{"\x1b[31mred\x1b[0m", false, 0},
		{"\x1b[2J\x1b[H", true, 0},
		{"\x1b[1;20r", true, 0},
		{"\x1b[?1049hfull screen", true, 1},
		{"\x1b[?1049h...\x1b[?1049l", true, -1},
		{"\x1bc", true, 0},
		{"cut \x1b[3", true, 0},
	}
	for _, c := range cases {
		if touched, alt := scanScreen([]byte(c.in)); touched != c.touched || alt != c.alt {
			t.Errorf("scanScreen(%q) = %v %d", c.in, touched, alt)
		}
	}
	for in, want := range map[string]bool{
		"done\x1b[0m": false, "x\x1b": true, "x\x1b[1;3": true,
		"x\x1b]0;title": true, "x\x1b]0;title\a": false, "no escapes": false,
	} {
		if got := endsMidEscape([]byte(in)); got != want {
			t.Errorf("endsMidEscape(%q) = %v", in, got)
		}
	}
}
