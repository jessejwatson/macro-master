package jobs

import "bytes"

// NextScreenSwitch finds the first switch to or from the alternate screen
// in b: where it is, how long, and whether it enters.
func NextScreenSwitch(b []byte) (at, n int, enter bool) {
	at = -1
	for _, mode := range []string{"1049", "1047", "47"} {
		for _, end := range []string{"h", "l"} {
			seq := "\x1b[?" + mode + end
			if i := bytes.Index(b, []byte(seq)); i >= 0 && (at < 0 || i < at) {
				at, n, enter = i, len(seq), end == "h"
			}
		}
	}
	return at, n, enter
}

// EndsOnAlt is true when the last screen switch in b enters the alternate
// screen.
func EndsOnAlt(b []byte) bool {
	alt := false
	for {
		i, n, enter := NextScreenSwitch(b)
		if i < 0 {
			return alt
		}
		alt, b = enter, b[i+n:]
	}
}
