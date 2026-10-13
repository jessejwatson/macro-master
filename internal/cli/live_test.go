package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"macro-master/internal/jobs"
)

// fakeJob saves a job that's running (this process stands in for its
// helper) or ended at the given time.
func fakeJob(t *testing.T, root string, ended *time.Time) *jobs.Meta {
	t.Helper()
	m, err := jobs.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Macro, m.Started, m.Ended = "personal/x", time.Now().Add(-time.Minute), ended
	if ended == nil {
		m.HelperPID = os.Getpid()
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	return m
}

func liveKeys(m liveModel, keys ...string) liveModel {
	for _, k := range keys {
		next, _ := m.Update(key(k))
		m = next.(liveModel)
	}
	return m
}

func TestLiveJobs(t *testing.T) {
	ta := newApp(t)
	ta.run(nil, "jobs") // opens the store
	root := filepath.Join(ta.Home, "jobs")
	since := time.Now()
	before := since.Add(-time.Hour)
	fakeJob(t, root, &before) // 1: ended before the view opened
	fakeJob(t, root, nil)     // 2
	three := fakeJob(t, root, nil)

	m := newLiveModel(ta.App, ta.newUI(false), since)
	if out := m.render(); strings.Contains(out, "\n  1 ") || !strings.Contains(out, "\n  2 ") || !strings.Contains(out, "2 running") {
		t.Fatalf("view:\n%s", out)
	}

	m = liveKeys(m, "a")
	if !m.selecting || m.cursor != "3" || !strings.Contains(m.render(), "› 3") {
		t.Fatalf("after a: selecting %v on %q\n%s", m.selecting, m.cursor, m.render())
	}
	m = liveKeys(m, "down", "down")
	if m.cursor != "2" {
		t.Errorf("down moved to %q", m.cursor)
	}
	m = liveKeys(m, "esc")
	if m.selecting || m.quitting {
		t.Errorf("esc while choosing: selecting %v, quitting %v", m.selecting, m.quitting)
	}

	// Job 3 ends while watching: it stays, and the view says so.
	m = liveKeys(m, "a")
	now := time.Now()
	three.Ended, three.ExitCode = &now, 2
	three.Save()
	next, _ := m.Update(liveTickMsg{})
	m = next.(liveModel)
	if !strings.Contains(m.render(), "\n  3 ") || m.note != "Job 3 failed (exit 2) after 1m0s" || m.cursor != "2" {
		t.Errorf("after job 3 ended: note %q, cursor %q\n%s", m.note, m.cursor, m.render())
	}

	m = liveKeys(m, "esc", "esc")
	if !m.quitting {
		t.Error("esc didn't quit")
	}
}

func TestLiveNeedsATerminal(t *testing.T) {
	ta := newApp(t)
	if code := ta.run(nil, "jobs", "--live"); code == 0 || !strings.Contains(ta.err.String(), "needs a terminal") {
		t.Errorf("exit %d, %q", code, ta.err)
	}
}

func TestLiveAttachComesBack(t *testing.T) {
	ta := newJobApp(t)
	ta.save(t, "personal/wait", "echo waiting\nsleep 30\n")
	if code := ta.run(nil, "-d", "wait"); code != 0 {
		t.Fatalf("-d: %s", ta.err)
	}

	p := startPty(t, ta.Home, "jobs", "--live")
	p.waitFor("Live jobs")
	p.send("a")
	p.waitFor("enter attach")
	p.send("\r")
	p.waitFor("WATCHING")
	p.waitFor("waiting")
	p.send("d")
	p.waitFor("Detached from job 1")
	// The first key after coming back reaches the view, not attach.
	p.send("q")
	if code := p.exit(); code != 0 {
		t.Errorf("exit = %d", code)
	}
	if !ta.job(t, "1").Running() {
		t.Error("detaching stopped the job")
	}
}
