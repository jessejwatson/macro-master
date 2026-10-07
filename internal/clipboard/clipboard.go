// Package clipboard reads the system clipboard by shelling out to the
// platform's paste tool.
package clipboard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// tools lists paste commands to try, in order, for each platform.
var tools = map[string][][]string{
	"darwin": {{"pbpaste"}},
	"linux": {
		{"wl-paste", "--no-newline"},
		{"xclip", "-selection", "clipboard", "-o"},
		{"xsel", "--clipboard", "--output"},
	},
}

// Read returns the clipboard text.
func Read() (string, error) {
	candidates := tools[runtime.GOOS]
	if runtime.GOOS == "linux" && os.Getenv("WAYLAND_DISPLAY") == "" {
		candidates = candidates[1:] // wl-paste needs a Wayland session
	}
	for _, argv := range candidates {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			continue
		}
		out, err := exec.Command(path, argv[1:]...).Output()
		if err != nil {
			return "", fmt.Errorf("%s failed (%v); try mm add <name> --stdin instead", argv[0], err)
		}
		return string(out), nil
	}
	switch runtime.GOOS {
	case "linux":
		return "", errors.New("no clipboard tool found; install wl-clipboard, xclip or xsel, or use mm add <name> --stdin")
	default:
		return "", errors.New("can't read the clipboard on this system; use mm add <name> --stdin")
	}
}
