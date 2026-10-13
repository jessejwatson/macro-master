package jobs

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLastLineIsWhatShowedLast(t *testing.T) {
	for _, c := range []struct{ log, want string }{
		{"", ""},
		{"first\r\nsecond\r\n\r\n  \r\n", "second"},
		{"\x1b[32mdone\x1b[0m\r\n", "done"},
		{"10%\r50%\r100%\r\n", "100%"},
		{"\x1b]0;title\x07ok\r\n", "ok"},
		{strings.Repeat("x", 300), strings.Repeat("x", 199) + "…"},
	} {
		p := t.TempDir() + "/output.log"
		os.WriteFile(p, []byte(c.log), 0o600)
		if got := lastLine(p); got != c.want {
			t.Errorf("lastLine(%q) = %q, want %q", c.log, got, c.want)
		}
	}
}

func TestNotificationSaysHowTheJobEnded(t *testing.T) {
	m, err := Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m.Macro, m.Name, m.Started, m.Ended = "personal/web/deploy", "web/deploy", now.Add(-4*time.Second), &now
	os.WriteFile(m.LogPath(), []byte("pushing\r\nall done\r\n"), 0o600)

	n := m.notification()
	if n.title != "✓ deploy finished after 4s" || n.subtitle != "job 1 · personal/web/deploy" || n.body != "all done" || !n.ok || !n.sound {
		t.Errorf("success: %+v", n)
	}
	m.ExitCode, m.JobName, m.Sound = 3, "prod", "success"
	n = m.notification()
	if n.title != "✗ prod failed (exit 3) after 4s" || n.subtitle != "job 1 (prod) · personal/web/deploy" || n.ok || n.sound {
		t.Errorf("failure with sound on success only: %+v", n)
	}
	m.Sound = "failure"
	if !m.notification().sound {
		t.Error("failure with sound on failure only is silent")
	}
}

func TestMacNotificationPlaysTheRightSound(t *testing.T) {
	n := notification{title: `say "hi"`, subtitle: "job 1", body: "out", ok: true, sound: true}
	script := notifyCommand("darwin", n).Args[2]
	if want := `display notification "out" with title "say \"hi\"" subtitle "job 1" sound name "Glass"`; script != want {
		t.Errorf("script = %s", script)
	}
	n.ok = false
	if script := notifyCommand("darwin", n).Args[2]; !strings.HasSuffix(script, `sound name "Basso"`) {
		t.Errorf("failure script = %s", script)
	}
	n.sound = false
	if script := notifyCommand("darwin", n).Args[2]; strings.Contains(script, "sound") {
		t.Errorf("silent script = %s", script)
	}
}
