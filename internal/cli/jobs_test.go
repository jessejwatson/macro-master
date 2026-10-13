package cli

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
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
		root := filepath.Join(ta.Home, "jobs")
		for _, m := range jobs.List(root) {
			if m.Running() && m.PID != 0 {
				syscall.Kill(-m.PID, syscall.SIGKILL)
			}
		}
		// Let the helpers record the end before the home is removed.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			running := false
			for _, m := range jobs.List(root) {
				running = running || m.Running()
			}
			if !running {
				break
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

	if code := ta.run(nil, "jobs"); code != 0 || !regexp.MustCompile(`1\s+-\s+✗ exit 4\s+personal/greet`).MatchString(ta.out.String()) {
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
		{[]string{"jobs", "x"}, "usage: mm jobs [--all | clear]"},
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
	p.send("\x1b") // esc back to watching
	p.send("d")
	if code := p.exit(); code != 0 {
		t.Errorf("detach exit = %d", code)
	}
	s := p.screen()
	if !strings.Contains(s, "Detached from job 1") || !strings.Contains(s, "\x1b[r") {
		t.Errorf("detach didn't restore the terminal:\n%q", s)
	}
	// The job's output was drawn on the alternate screen, which detaching
	// leaves, so it's gone from the terminal.
	in, out := strings.Index(s, "\x1b[?1049h"), strings.LastIndex(s, "\x1b[?1049l")
	if in < 0 || out < 0 || !(in < strings.Index(s, "hello bob") && strings.Index(s, "hello bob") < out && out < strings.Index(s, "Detached")) {
		t.Errorf("output not kept to the alternate screen:\n%q", s)
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

func TestForegroundRunIsAnAttachedJob(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/quick", "echo \"quick $1\"\nexit 3\n")
	ta.save(t, "personal/ask", "read -r -p 'name? ' n\necho \"hello $n\"\nsleep 30\n")
	root := filepath.Join(ta.Home, "jobs")

	// One that ends by itself leaves its output and exit status, and no job.
	p := startPty(t, ta.Home, "quick", "one")
	if code := p.exit(); code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
	s := p.screen()
	if !strings.Contains(s[max(strings.LastIndex(s, "\x1b[?1049l"), 0):], "quick one") {
		t.Errorf("output not left in the terminal:\n%q", s)
	}
	if left := jobs.List(root); len(left) != 0 {
		t.Errorf("finished foreground job kept: %v", left)
	}

	// Keys go straight to it; esc then d detaches and leaves it running.
	p = startPty(t, ta.Home, "ask")
	p.waitFor("TYPING")
	p.waitFor("name? ")
	p.send("bob\r")
	p.waitFor("hello bob")
	p.send("\x1b")
	p.waitFor("WATCHING")
	p.send("d")
	if code := p.exit(); code != 0 {
		t.Errorf("detach exit = %d", code)
	}
	// Inline, not on the alternate screen, so the output stays put.
	s = p.screen()
	if strings.Contains(s, "\x1b[?1049") || !strings.Contains(s, "\x1b[r") ||
		!(strings.Index(s, "hello bob") < strings.LastIndex(s, "Detached from job")) {
		t.Errorf("foreground output not inline:\n%q", s)
	}
	if l := jobs.List(root); len(l) != 1 || !l[0].Running() {
		t.Errorf("detached job not running: %v", l)
	}
}

func TestConfirmTakesOneKey(t *testing.T) {
	ta := newApp(t)
	ta.save(t, "personal/keep", "echo\n")
	ta.save(t, "personal/gone", "echo\n")

	// n on its own answers; arrow keys are ignored.
	p := startPty(t, ta.Home, "rm", "keep")
	p.waitFor("[y/N]")
	p.send("\x1b[A")
	p.send("n")
	if code := p.exit(); code == 0 || !ta.exists("personal/keep") {
		t.Errorf("n: exit %d, kept %v", code, ta.exists("personal/keep"))
	}
	p = startPty(t, ta.Home, "rm", "gone")
	p.waitFor("[y/N]")
	p.send("y")
	if code := p.exit(); code != 0 || ta.exists("personal/gone") {
		t.Errorf("y: exit %d, still there %v\n%q", code, ta.exists("personal/gone"), p.screen())
	}
}

func TestWatchingSkipsEscapeSequences(t *testing.T) {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	ta := newApp(t)
	at := &attachment{app: ta.App, job: &jobs.Meta{ID: "1", Macro: "personal/x"}, out: devnull, u: ta.newUI(false), rows: 24, cols: 80}

	// The terminal answering the job's output, focus events and special
	// keys: none of their letters are keys.
	for _, in := range []string{
		"\x1b[0n", "\x1b[12;40R", "\x1b[?62;22c", "\x1b[I", "\x1b[O",
		"\x1bP>|iTerm2 3.6.1\x1b\\", "\x1b]11;rgb:1e1e/1e1e/1e1e\x07", "\x1bOP", "\x1b[A",
		"\x1b[0n\x1bP>|iTerm2\x1b\\\x1b[I",
	} {
		if got := at.keys([]byte(in)); got != "" || at.typing || at.naming {
			t.Errorf("%q: result %q, typing %v, naming %v", in, got, at.typing, at.naming)
		}
	}
	if got := at.keys([]byte("\x1b[0nd")); got != "detach" {
		t.Errorf("d after a report: %q", got)
	}
	// Naming, a report is skipped and what follows is typed.
	at.keys([]byte("n"))
	at.keys([]byte("ab\x1b[Ic"))
	if !at.naming || string(at.name) != "abc" {
		t.Errorf("naming %v, name %q", at.naming, string(at.name))
	}
	if at.keys([]byte("\x1b")); at.naming {
		t.Error("esc didn't cancel naming")
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

func TestJobNames(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/long", "echo up\nsleep 30\n")

	if code := ta.run(nil, "-d", "-n", "web", "long"); code != 0 {
		t.Fatalf("-d -n: %s", ta.err)
	}
	if !strings.Contains(ta.err.String(), "Started job 1 (web)") || !strings.Contains(ta.err.String(), "mm attach web") {
		t.Errorf("start message = %q", ta.err)
	}
	// -n alone detaches too; names must be free and well formed.
	for args, want := range map[string]string{
		"-n web long":  "job 1 is already called web",
		"-n 42 long":   "can't be just digits",
		"-n a/b long":  "can only use letters",
		"--name= long": "--name= needs a job name",
		"-n  long":     "-n needs a job name",
		"-n " + strings.Repeat("x", 33) + " long": "1 to 32 characters",
		"-d -n":           "-n needs a job name",
		"run -n web long": "already called web",
	} {
		if code := ta.run(nil, strings.Split(args, " ")...); code == 0 || !strings.Contains(ta.err.String(), want) {
			t.Errorf("%s: exit %d, err %q, want %q", args, code, ta.err, want)
		}
	}
	if code := ta.run(nil, "--name=db", "long"); code != 0 {
		t.Fatalf("--name=: %s", ta.err)
	}
	if ta.job(t, "2").JobName != "db" {
		t.Error("--name= not saved")
	}

	// Rename in mm attach with n, then find the job by its new name.
	p := startPty(t, ta.Home, "attach", "web")
	p.waitFor("WATCHING")
	p.send("n")
	p.waitFor("job name:")
	p.send("\x15api\r")
	p.waitFor("named api")
	p.send("n")
	p.send("\x15db\r") // taken by job 2
	p.waitFor("not renamed: job 2 is already called db")
	p.send("d")
	if code := p.exit(); code != 0 {
		t.Fatalf("attach exit %d", code)
	}
	if !strings.Contains(p.screen(), "mm attach api to return") {
		t.Errorf("detach hint doesn't use the name:\n%q", p.screen())
	}
	if m := ta.job(t, "1"); m.JobName != "api" {
		t.Errorf("name = %q", m.JobName)
	}
	if code := ta.run(nil, "kill", "api"); code != 0 || !strings.Contains(ta.err.String(), "Stopped job 1 (api)") {
		t.Errorf("kill by name: %q", ta.err)
	}
	// The name survives the helper recording the job's end.
	if m := ta.job(t, "1"); m.JobName != "api" {
		t.Errorf("name after end = %q", m.JobName)
	}
	ta.run(nil, "jobs")
	if !regexp.MustCompile(`(?m)^1\s+api\s+✗ terminated\s+personal/long`).MatchString(ta.out.String()) {
		t.Errorf("jobs:\n%s", ta.out)
	}
}

func TestJobsTableLinesUp(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/ok", "true\n")
	ta.save(t, "personal/bad", "exit 255\n")
	ta.run(nil, "-d", "ok")
	ta.waitEnd(t, "1")
	ta.run(nil, "-d", "-n", "x", "bad")
	ta.waitEnd(t, "2")
	ta.StyleOut = true
	ta.run(nil, "jobs")
	lines := strings.Split(strings.TrimRight(plain(ta.out.String()), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("jobs:\n%s", ta.out)
	}
	col := func(l, s string) int { return lipgloss.Width(l[:strings.Index(l, s)]) }
	if col(lines[0], "MACRO") != col(lines[1], "personal/") || col(lines[1], "personal/") != col(lines[2], "personal/") ||
		col(lines[0], "STATUS") != col(lines[2], "✓") {
		t.Errorf("columns don't line up:\n%s", strings.Join(lines, "\n"))
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
	for in, want := range map[string]bool{
		"plain text\r\n": false, "\x1b[31mred\x1b[0m": false, "\x1b[2J\x1b[H": true,
		"\x1b[1;20r": true, "\x1bc": true, "cut \x1b[3": true,
	} {
		if got := scanScreen([]byte(in)); got != want {
			t.Errorf("scanScreen(%q) = %v", in, got)
		}
	}
	if got := string(ownScreen([]byte("a\x1b[?1049hb\x1b[?1049lc\x1b[?25l"))); got != "a\x1b[H\x1b[2Jb\x1b[H\x1b[2Jc\x1b[?25l" {
		t.Errorf("ownScreen = %q", got)
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

func TestRenameReportsAnOlderHelper(t *testing.T) {
	ta := newJobApp(t)
	ta.run(nil, "jobs") // open the store
	m, err := jobs.Create(filepath.Join(ta.Home, "jobs"))
	if err != nil {
		t.Fatal(err)
	}
	m.HelperPID, m.Macro = os.Getpid(), "personal/old"
	m.Save()
	// A helper from before names existed: it reads requests and ignores them.
	ln, err := net.Listen("unix", m.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	conn, err := jobs.Dial(m)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	at := &attachment{app: ta.App, job: m, conn: conn}
	at.saveName("web")
	if !strings.Contains(at.note, "started by an older mm") || at.job.JobName != "" {
		t.Errorf("note = %q, name = %q", at.note, at.job.JobName)
	}
}

func TestJobsListsTheNewestUnlessAll(t *testing.T) {
	ta := newJobApp(t)
	root := filepath.Join(ta.Home, "jobs")
	now := time.Now()
	for i := 1; i <= jobsShown+3; i++ {
		m, err := jobs.Create(root)
		if err != nil {
			t.Fatal(err)
		}
		m.Macro, m.Started = "personal/x", now
		if i == 1 {
			m.HelperPID = os.Getpid() // an old job that's still running
			t.Cleanup(func() { os.RemoveAll(m.JobDir()) })
		} else {
			m.Ended = &now
		}
		if err := m.Save(); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		var out []string
		for _, sm := range regexp.MustCompile(`(?m)^(\d+)\s`).FindAllStringSubmatch(ta.out.String(), -1) {
			out = append(out, sm[1])
		}
		return out
	}

	ta.run(nil, "jobs")
	got := ids()
	if len(got) != jobsShown || got[0] != "13" || got[jobsShown-2] != "5" || got[jobsShown-1] != "1" {
		t.Errorf("jobs listed %v, want 13..5 and running job 1", got)
	}
	if !strings.Contains(ta.err.String(), "3 older jobs not shown") {
		t.Errorf("jobs stderr = %q", ta.err)
	}

	ta.run(nil, "jobs", "--all")
	if got := ids(); len(got) != jobsShown+3 || ta.err.Len() != 0 {
		t.Errorf("jobs --all listed %v, stderr %q", got, ta.err)
	}

	// A 7-row terminal fits the header, 3 jobs, the note, the command and
	// the next prompt. The running job takes one of the 3.
	ta.Height = 7
	ta.run(nil, "jobs")
	if got := ids(); !slices.Equal(got, []string{"13", "12", "1"}) || !strings.Contains(ta.err.String(), "10 older jobs not shown") {
		t.Errorf("jobs in 7 rows listed %v, stderr %q", got, ta.err)
	}
}

func TestFitJobNames(t *testing.T) {
	headers := []string{"ID", "NAME", "MACRO"}
	rows := [][]string{{"1", "pull-ctrl-endpoint-img", "personal/x"}, {"2", "-", "personal/x"}}
	// 2 + 22 + 10 + two gaps of 2 = 38 columns; 30 leaves 14 for the name.
	fitJobNames(headers, rows, 30)
	if rows[0][1] != "pull-ctrl-end…" || rows[1][1] != "-" {
		t.Errorf("rows = %q", rows)
	}
	fitJobNames(headers, rows, 5)
	if rows[0][1] != "pull-…" {
		t.Errorf("narrowest name = %q", rows[0][1])
	}
}
