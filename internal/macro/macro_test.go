package macro

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	content := "#!/usr/bin/env bash\n# description: Flush DNS\n# mode: Source\n# a normal comment\necho hi\n"
	m := Parse(content)
	if m.Shebang != "#!/usr/bin/env bash" {
		t.Errorf("shebang = %q", m.Shebang)
	}
	if m.Description != "Flush DNS" {
		t.Errorf("description = %q", m.Description)
	}
	if m.Mode != "source" {
		t.Errorf("mode = %q", m.Mode)
	}
	if m.Body != "# a normal comment\necho hi\n" {
		t.Errorf("body = %q", m.Body)
	}
}

func TestParseNoHeader(t *testing.T) {
	m := Parse("ls -la")
	if m.Shebang != "" || m.Description != "" || m.Body != "ls -la" {
		t.Errorf("got %+v", m)
	}
}

func TestCompose(t *testing.T) {
	got := Compose("echo hi", "Say hi")
	want := "#!/usr/bin/env bash\n# description: Say hi\necho hi\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got = Compose("#!/bin/zsh\necho hi", "")
	if got != "#!/bin/zsh\necho hi\n" {
		t.Errorf("kept shebang: got %q", got)
	}
}

func TestInterpreter(t *testing.T) {
	cases := map[string][]string{
		"":                           {"bash"},
		"#!/bin/sh":                  {"/bin/sh"},
		"#!/usr/bin/env python3":     {"/usr/bin/env", "python3"},
		"#!/usr/bin/env -S bash -eu": {"/usr/bin/env", "-S bash -eu"},
	}
	for shebang, want := range cases {
		if got := (Macro{Shebang: shebang}).Interpreter(); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", shebang, got, want)
		}
	}
}

func TestValidateMacroName(t *testing.T) {
	for _, ok := range []string{"deploy-web", "a", "x_1", "9lives"} {
		if err := ValidateMacroName(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Deploy", "-x", "a b", "a/b", "add", "ls", "x.sh"} {
		if err := ValidateMacroName(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestClean(t *testing.T) {
	in := "\r\n\n$ brew update  \r\n% brew upgrade\t\r\n  echo $HOME\n\n\n"
	want := "brew update\nbrew upgrade\n  echo $HOME"
	if got := Clean(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPlaceholders(t *testing.T) {
	body := "ssh {{user:root}}@{{host}} -p {{port:22}} && echo {{host:ignored}} {{ user }}"
	got := Placeholders(body)
	want := []Placeholder{
		{Name: "user", Default: "root", HasDefault: true},
		{Name: "host", Default: "ignored", HasDefault: true},
		{Name: "port", Default: "22", HasDefault: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFill(t *testing.T) {
	body := "ssh {{user:root}}@{{host}} {{other}}"
	got := Fill(body, map[string]string{"user": "admin", "host": "web1"})
	if got != "ssh admin@web1 {{other}}" {
		t.Errorf("got %q", got)
	}
	// Empty default is a real default.
	if ph := Placeholders("{{x:}}"); !ph[0].HasDefault || ph[0].Default != "" {
		t.Errorf("empty default: %+v", ph)
	}
}

func TestSplitArgs(t *testing.T) {
	ph := []Placeholder{{Name: "host"}}
	vals, rest := SplitArgs([]string{"host=web1", "--force", "other=1", "host=web2=x"}, ph)
	if vals["host"] != "web2=x" {
		t.Errorf("host = %q", vals["host"])
	}
	if !reflect.DeepEqual(rest, []string{"--force", "other=1"}) {
		t.Errorf("rest = %q", rest)
	}
}

func TestBlobHashMatchesGit(t *testing.T) {
	// git hash-object of "echo hi\n"
	if got := BlobHash("echo hi\n"); got != "8b2fe5434fec16870a71cd8b272c7fcf6d352536" {
		t.Errorf("got %s", got)
	}
}
