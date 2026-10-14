package cli

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestEditLine(t *testing.T) {
	const (
		left, right = "\x1b[D", "\x1b[C"
		home, end   = "\x1b[H", "\x1b[F"
		del         = "\x1b[3~"
		wordLeft    = "\x1b[1;5D"
		optLeft     = "\x1bb"
	)
	for _, c := range []struct{ keys, want string }{
		{"hello\r", "hello"},
		{"helo" + left + "l\r", "hello"},
		{"world" + home + "hello \r", "hello world"},
		{"ab" + home + right + "X" + end + "!\r", "aXb!"},
		{"abc" + left + "\x7f\r", "ac"},
		{"abc" + home + del + "\r", "bc"},
		{"one two" + wordLeft + "big \r", "one big two"},
		{"one two" + optLeft + "\x17\r", "two"},
		{"one two\x17\r", "one "},
		{"abc" + left + "\x0b\r", "ab"},
		{"abc" + left + "\x15\r", "c"},
		{"\x7f" + left + "x" + "\x1b[A\r", "x"}, // nothing to delete; up is ignored
		{"héllo" + left + left + left + "\x7f\r", "hllo"},
	} {
		got, err := editLine(bufio.NewReader(strings.NewReader(c.keys)), io.Discard)
		if err != nil || got != c.want {
			t.Errorf("%q: %q, %v; want %q", c.keys, got, err, c.want)
		}
	}
	for keys, want := range map[string]error{"\x03": exitCode(130), "\x04": errCancelled, "ab\x1b": errCancelled, "ab": errCancelled} {
		if _, err := editLine(bufio.NewReader(strings.NewReader(keys)), io.Discard); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", keys, err, want)
		}
	}
}
