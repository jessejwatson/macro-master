package macro

import "strings"

// Clean tidies pasted text: CRLF to LF, trailing whitespace trimmed from
// each line, leading and trailing blank lines dropped, and a leading "$ " or
// "% " shell prompt removed from lines copied out of docs.
func Clean(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, " \t")
		if rest, ok := strings.CutPrefix(line, "$ "); ok {
			line = rest
		} else if rest, ok := strings.CutPrefix(line, "% "); ok {
			line = rest
		}
		lines[i] = line
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
