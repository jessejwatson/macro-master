package cli

import (
	"fmt"
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/term"
)

// Colours are the terminal's own 16 ANSI colours, so mm follows the user's
// theme instead of fighting it.
var (
	colAccent = lipgloss.Color("5") // magenta: libraries, prompts, headings
	colFolder = lipgloss.Color("4") // blue
	colCmd    = lipgloss.Color("6") // cyan: commands and diff hunks
	colOK     = lipgloss.Color("2")
	colWarn   = lipgloss.Color("3")
	colBad    = lipgloss.Color("1")
	colBorder = lipgloss.Color("8")
)

// icons are the glyphs mm decorates output with. Empty means none.
type icons struct {
	lib, synced, folder, macro, fav, ok, warn, err, lock string
}

var (
	unicodeIcons = icons{lib: "◆", synced: "⟳", fav: "★", ok: "✓", warn: "!", err: "✗"}
	// MM_ICONS=nerd, for terminals with a Nerd Font.
	nerdIcons = icons{
		lib: "", synced: "", folder: "", macro: "",
		fav: "", ok: "", warn: "", err: "", lock: "",
	}
)

// ui renders output for one stream. When off, every helper returns its
// input unchanged, so pipes, NO_COLOR, TERM=dumb and tests get plain text.
type ui struct {
	on    bool
	icons icons
	width int
}

// styleWanted says whether f should get colours and panels.
func styleWanted(f *os.File) bool {
	return isTerminal(f) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// termWidth is the terminal's width, or 80 when it can't be told.
func termWidth() int {
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if w, _, err := term.GetSize(f.Fd()); err == nil && w > 0 {
			return w
		}
	}
	return 80
}

func (a *App) newUI(on bool) ui {
	u := ui{on: on, icons: unicodeIcons, width: a.Width}
	if a.Icons == "nerd" {
		u.icons = nerdIcons
	}
	if u.width <= 0 {
		u.width = 80
	}
	return u
}

// outUI styles stdout; errUI styles stderr.
func (a *App) outUI() ui { return a.newUI(a.StyleOut) }
func (a *App) errUI() ui { return a.newUI(a.StyleErr) }

func (u ui) render(s lipgloss.Style, text string) string {
	if !u.on || text == "" {
		return text
	}
	return s.Render(text)
}

func (u ui) fg(c color.Color, text string) string {
	return u.render(lipgloss.NewStyle().Foreground(c), text)
}

func (u ui) bold(text string) string  { return u.render(lipgloss.NewStyle().Bold(true), text) }
func (u ui) faint(text string) string { return u.render(lipgloss.NewStyle().Faint(true), text) }
func (u ui) accent(text string) string {
	return u.render(lipgloss.NewStyle().Foreground(colAccent).Bold(true), text)
}

// icon returns glyph followed by a space, or "" when styling is off or the
// icon set has no such glyph.
func (u ui) icon(glyph string, c color.Color) string {
	if !u.on || glyph == "" {
		return ""
	}
	return u.fg(c, glyph) + " "
}

// panel draws body in a rounded box with an optional bold title line.
// Lines wider than the terminal wrap inside the box.
func (u ui) panel(title, body string, border color.Color) string {
	var lines []string
	if title != "" {
		lines = append(lines, title)
	}
	if body != "" {
		if title != "" {
			lines = append(lines, "")
		}
		lines = append(lines, strings.TrimRight(body, "\n"))
	}
	content := strings.Join(lines, "\n")
	s := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1)
	if lipgloss.Width(content)+4 > u.width {
		s = s.Width(u.width)
	}
	return s.Render(content)
}

// code shows macro text with line numbers, comments dimmed and
// placeholders highlighted.
func (u ui) code(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(u.faint(fmt.Sprintf("%3d ", i+1)))
		b.WriteString(u.codeLine(l))
	}
	return b.String()
}

func (u ui) codeLine(l string) string {
	if strings.HasPrefix(strings.TrimSpace(l), "#") {
		return u.faint(l)
	}
	var b strings.Builder
	for {
		start := strings.Index(l, "{{")
		end := strings.Index(l[max(start, 0):], "}}")
		if start < 0 || end < 0 {
			b.WriteString(l)
			return b.String()
		}
		end += start + 2
		b.WriteString(l[:start])
		b.WriteString(u.accent(l[start:end]))
		l = l[end:]
	}
}

// diff colours a unified diff.
func (u ui) diff(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"),
			strings.HasPrefix(l, "diff "), strings.HasPrefix(l, "index "):
			lines[i] = u.faint(l)
		case strings.HasPrefix(l, "@@"):
			lines[i] = u.fg(colCmd, l)
		case strings.HasPrefix(l, "+"):
			lines[i] = u.fg(colOK, l)
		case strings.HasPrefix(l, "-"):
			lines[i] = u.fg(colBad, l)
		}
	}
	return strings.Join(lines, "\n")
}

// done reports a finished action on stderr.
func (a *App) done(format string, args ...any) {
	u := a.errUI()
	fmt.Fprintf(a.Stderr, "%s%s\n", u.icon(u.icons.ok, colOK), fmt.Sprintf(format, args...))
}

// nothing reports that the user backed out and nothing changed.
func (a *App) nothing(msg string) {
	fmt.Fprintln(a.Stderr, a.errUI().faint(msg))
}

// notef prints an "mm: ..." notice on stderr.
func (a *App) notef(format string, args ...any) {
	u := a.errUI()
	msg := fmt.Sprintf(format, args...)
	if u.on {
		fmt.Fprintf(a.Stderr, "%s%s\n", u.fg(colCmd, "› "), msg)
		return
	}
	fmt.Fprintf(a.Stderr, "mm: %s\n", msg)
}

// warnf prints an "mm: warning: ..." on stderr.
func (a *App) warnf(format string, args ...any) {
	u := a.errUI()
	msg := fmt.Sprintf(format, args...)
	if u.on {
		fmt.Fprintf(a.Stderr, "%s%s %s\n", u.icon(u.icons.warn, colWarn), u.fg(colWarn, "warning:"), msg)
		return
	}
	fmt.Fprintf(a.Stderr, "mm: warning: %s\n", msg)
}
