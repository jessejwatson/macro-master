package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// askEdit asks a question on the terminal with a line you can edit: arrows,
// home and end move, word moves and deletes work as in a shell, enter
// answers, and ctrl+c, ctrl+d on an empty line or esc cancel.
func (a *App) askEdit(question string) (string, error) {
	a.printQuestion(question)
	state, err := term.MakeRaw(a.promptTTY.Fd())
	if err != nil {
		return a.readAnswer()
	}
	line, err := editLine(a.Prompts, a.Stderr)
	term.Restore(a.promptTTY.Fd(), state)
	fmt.Fprintln(a.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// lineEditor is the line being typed and where the cursor is in it.
type lineEditor struct {
	buf []rune
	pos int
	out io.Writer
}

// editLine reads keys from a raw terminal and echoes the line to out until
// enter, returning what was typed.
func editLine(in *bufio.Reader, out io.Writer) (string, error) {
	e := &lineEditor{out: out}
	for {
		r, _, err := in.ReadRune()
		if err != nil {
			return "", errCancelled
		}
		switch r {
		case '\r', '\n':
			return string(e.buf), nil
		case 0x03: // ctrl+c
			return "", exitCode(130)
		case 0x04: // ctrl+d
			if len(e.buf) == 0 {
				return "", errCancelled
			}
			e.edit(e.pos, e.pos+1, e.pos)
		case 0x01: // ctrl+a
			e.move(0)
		case 0x05: // ctrl+e
			e.move(len(e.buf))
		case 0x02: // ctrl+b
			e.move(e.pos - 1)
		case 0x06: // ctrl+f
			e.move(e.pos + 1)
		case 0x7f, 0x08: // backspace
			e.edit(e.pos-1, e.pos, e.pos-1)
		case 0x15: // ctrl+u
			e.edit(0, e.pos, 0)
		case 0x0b: // ctrl+k
			e.edit(e.pos, len(e.buf), e.pos)
		case 0x17: // ctrl+w
			w := e.wordLeft()
			e.edit(w, e.pos, w)
		case 0x1b:
			if in.Buffered() == 0 {
				return "", errCancelled // esc on its own
			}
			e.escape(in)
		default:
			if unicode.IsPrint(r) {
				e.insert(r)
			}
		}
	}
}

// escape handles a key that arrives as an escape sequence: arrows, home,
// end, delete, and their word-wise forms with alt or ctrl.
func (e *lineEditor) escape(in *bufio.Reader) {
	b, _ := in.ReadByte()
	switch b {
	case 'b': // alt+b, and option+left in many terminals
		e.move(e.wordLeft())
		return
	case 'f':
		e.move(e.wordRight())
		return
	case 0x7f: // alt+backspace
		w := e.wordLeft()
		e.edit(w, e.pos, w)
		return
	case '[', 'O':
	default:
		return
	}
	// A CSI or SS3 sequence: parameters, then one final byte.
	var params []byte
	final := byte(0)
	for in.Buffered() > 0 {
		c, _ := in.ReadByte()
		if c >= 0x40 && c <= 0x7e {
			final = c
			break
		}
		params = append(params, c)
	}
	p := string(params)
	word := strings.HasSuffix(p, ";3") || strings.HasSuffix(p, ";5") || strings.HasSuffix(p, ";9")
	switch final {
	case 'D':
		if word {
			e.move(e.wordLeft())
		} else {
			e.move(e.pos - 1)
		}
	case 'C':
		if word {
			e.move(e.wordRight())
		} else {
			e.move(e.pos + 1)
		}
	case 'H':
		e.move(0)
	case 'F':
		e.move(len(e.buf))
	case '~':
		switch strings.SplitN(p, ";", 2)[0] {
		case "1", "7":
			e.move(0)
		case "4", "8":
			e.move(len(e.buf))
		case "3":
			e.edit(e.pos, e.pos+1, e.pos)
		}
	}
	// Up, down and anything else: ignored.
}

func (e *lineEditor) wordLeft() int {
	i := e.pos
	for i > 0 && unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.buf[i-1]) {
		i--
	}
	return i
}

func (e *lineEditor) wordRight() int {
	i := e.pos
	for i < len(e.buf) && unicode.IsSpace(e.buf[i]) {
		i++
	}
	for i < len(e.buf) && !unicode.IsSpace(e.buf[i]) {
		i++
	}
	return i
}

func width(rs []rune) int { return ansi.StringWidth(string(rs)) }

// move puts the cursor at i.
func (e *lineEditor) move(i int) {
	i = max(0, min(i, len(e.buf)))
	if i < e.pos {
		cursorLeft(e.out, width(e.buf[i:e.pos]))
	} else if i > e.pos {
		fmt.Fprint(e.out, string(e.buf[e.pos:i]))
	}
	e.pos = i
}

func (e *lineEditor) insert(r rune) {
	e.buf = append(e.buf[:e.pos], append([]rune{r}, e.buf[e.pos:]...)...)
	tail := e.buf[e.pos:]
	fmt.Fprint(e.out, string(tail))
	e.pos++
	cursorLeft(e.out, width(e.buf[e.pos:]))
}

// edit removes buf[from:to] and leaves the cursor at pos, redrawing from
// the first change.
func (e *lineEditor) edit(from, to, pos int) {
	from, to = max(0, from), min(to, len(e.buf))
	if from >= to {
		return
	}
	e.move(from)
	gone := width(e.buf[from:to])
	e.buf = append(e.buf[:from], e.buf[to:]...)
	tail := e.buf[from:]
	fmt.Fprint(e.out, string(tail)+strings.Repeat(" ", gone))
	cursorLeft(e.out, width(tail)+gone)
	e.pos = from
	e.move(pos)
}

func cursorLeft(w io.Writer, n int) {
	if n > 0 {
		fmt.Fprintf(w, "\x1b[%dD", n)
	}
}
