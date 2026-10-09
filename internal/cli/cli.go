// Package cli implements the mm commands.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"macro-master/internal/clipboard"
	"macro-master/internal/hosts"
	"macro-master/internal/jobs"
	"macro-master/internal/store"
)

// App holds everything a command needs, so tests can swap the outside world.
type App struct {
	Version string

	Stdin  io.Reader // macro input, and content for add --stdin
	Stdout io.Writer
	Stderr io.Writer

	// Prompts reads answers to questions. Nil means there is no terminal to
	// ask, so commands that need confirmation refuse.
	Prompts *bufio.Reader
	// Interactive is true when stdin and stdout are both terminals, which
	// the picker needs.
	Interactive bool
	// StyleOut and StyleErr turn on colours and panels for stdout and
	// stderr. Icons is "nerd" for Nerd Font glyphs; Width is the terminal's.
	StyleOut, StyleErr bool
	Icons              string
	Width              int

	Home          string
	ReadClipboard func() (string, error)
	Getenv        func(string) string
	LookPath      func(string) (string, error)

	// Spawn starts a detached background mm with args. Nil uses the real
	// binary; tests replace it.
	Spawn func(args []string) error
	// StartJob starts the helper for the detached job in dir, with extra
	// environment. Nil uses the real binary; tests replace it.
	StartJob func(dir string, env ...string) error
	// ReadSecret reads a token without echo. Nil uses stty.
	ReadSecret func(prompt string) (string, error)
	Hosts      *hosts.Client
	Tokens     *hosts.Tokens

	store        *store.Store
	gitAvailable *bool
}

// exitCode carries a specific exit status up to Run without a message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// errCancelled is returned when the user declines a prompt.
var errCancelled = exitCode(1)

// NewApp wires an App to the real process environment.
func NewApp(version string) *App {
	a := &App{
		Version:       version,
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		ReadClipboard: clipboard.Read,
		Getenv:        os.Getenv,
		StyleOut:      styleWanted(os.Stdout),
		StyleErr:      styleWanted(os.Stderr),
		Icons:         os.Getenv("MM_ICONS"),
		Width:         termWidth(),
	}
	stdinTTY, stdoutTTY := isTerminal(os.Stdin), isTerminal(os.Stdout)
	a.Interactive = stdinTTY && stdoutTTY
	if stdinTTY {
		a.Prompts = bufio.NewReader(os.Stdin)
	} else if tty, err := os.Open("/dev/tty"); err == nil {
		// stdin is busy (a pipe or --stdin content); ask on the terminal.
		a.Prompts = bufio.NewReader(tty)
	}
	return a
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// Run executes mm with args (without the program name) and returns the
// process exit code.
func (a *App) Run(args []string) int {
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	err := a.run(args)
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	default:
		if u := a.errUI(); u.on {
			fmt.Fprintf(a.Stderr, "%s%s\n", u.icon(u.icons.err, colBad), u.fg(colBad, "error:")+" "+err.Error())
		} else {
			fmt.Fprintf(a.Stderr, "mm: %v\n", err)
		}
		return 1
	}
}

func (a *App) run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "help", "-h", "--help":
			fmt.Fprint(a.Stdout, a.outUI().help(helpText))
			return nil
		case "--version", "version":
			u := a.outUI()
			fmt.Fprintf(a.Stdout, "%s %s\n", u.accent("mm"), a.Version)
			return nil
		}
	}
	if len(args) > 0 && args[0] == "__complete" {
		// Completion must stay instant: no pruning, notices or sync.
		home, err := a.homeDir()
		if err != nil {
			return err
		}
		s, err := store.Open(home)
		if err != nil {
			return err
		}
		for _, c := range complete(s, args[1:]) {
			fmt.Fprintln(a.Stdout, c)
		}
		return nil
	}
	if len(args) > 0 && args[0] == "__job" {
		if len(args) != 2 {
			return errors.New("usage: mm __job <dir>")
		}
		return jobs.Helper(args[1])
	}
	if len(args) > 0 && args[0] == "__preview" {
		return a.cmdPreview(args[1:])
	}
	if len(args) > 0 && (args[0] == "init" || args[0] == "completion") {
		return a.cmdShellScript(args[0], args[1:])
	}
	if err := a.openStore(); err != nil {
		return err
	}
	background := len(args) >= 2 && args[0] == "sync" && args[1] == "--background"
	if !background {
		a.showNotices()
		if len(args) == 0 || args[0] != "sync" {
			a.maybeBackgroundSync()
		}
	}
	if len(args) == 0 {
		return a.cmdPick()
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		return a.cmdAdd(rest)
	case "edit":
		return a.cmdEdit(rest)
	case "show":
		return a.cmdShow(rest)
	case "print":
		return a.cmdPrint(rest)
	case "rm":
		return a.cmdRm(rest)
	case "mv":
		return a.cmdMv(rest)
	case "rename":
		return a.cmdRename(rest)
	case "ls":
		return a.cmdLs(rest)
	case "fav":
		return a.cmdFav(rest)
	case "lib":
		return a.cmdLib(rest)
	case "run":
		detach := len(rest) > 0 && isDetachFlag(rest[0])
		if detach {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return errors.New("mm run needs a macro name")
		}
		if detach {
			return a.cmdRunDetached(rest[0], rest[1:])
		}
		return a.cmdRun(rest[0], rest[1:])
	case "-d", "--detach":
		if len(rest) == 0 {
			return fmt.Errorf("%s needs a macro name: mm %s <name>", cmd, cmd)
		}
		return a.cmdRunDetached(rest[0], rest[1:])
	case "jobs":
		return a.cmdJobs(rest)
	case "attach":
		return a.cmdAttach(rest)
	case "kill":
		return a.cmdKill(rest)
	case "config":
		return a.cmdConfig(rest)
	case "sync":
		return a.cmdSync(rest)
	case "auth":
		return a.cmdAuth(rest)
	case "trust":
		return a.cmdTrust(rest)
	}
	if strings.HasPrefix(cmd, "-") {
		return fmt.Errorf("unknown option %s; run mm help for usage", cmd)
	}
	return a.cmdRun(cmd, rest)
}

func isDetachFlag(s string) bool { return s == "-d" || s == "--detach" }

func (a *App) homeDir() (string, error) {
	if a.Home != "" {
		return a.Home, nil
	}
	return store.HomeDir()
}

func (a *App) openStore() error {
	home, err := a.homeDir()
	if err != nil {
		return err
	}
	s, err := store.Open(home)
	if err != nil {
		return err
	}
	for _, w := range s.Warnings {
		a.warnf("%s", w)
	}
	a.store = s
	return s.Prune()
}

// ask prints a question and returns the trimmed answer.
func (a *App) ask(question string) (string, error) {
	if a.Prompts == nil {
		return "", errors.New("this needs a terminal to answer questions")
	}
	if u := a.errUI(); u.on {
		// A trailing [default] or [y/N] hint is dimmed.
		q, hint := strings.TrimRight(question, " "), ""
		if i := strings.LastIndex(q, " ["); i >= 0 && (strings.HasSuffix(q, "]") || strings.HasSuffix(q, "]:")) {
			q, hint = q[:i], q[i:]
		}
		question = u.accent("? ") + u.bold(q) + u.faint(hint) + " "
	}
	fmt.Fprint(a.Stderr, question)
	line, err := a.Prompts.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		fmt.Fprintln(a.Stderr)
		return "", errCancelled
	}
	return strings.TrimSpace(line), nil
}

// confirm asks a yes/no question; def is the answer for a bare Enter.
func (a *App) confirm(question string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	ans, err := a.ask(question + " " + hint + " ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(ans) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

func (a *App) lookPath(name string) (string, error) {
	if a.LookPath != nil {
		return a.LookPath(name)
	}
	return lookPathReal(name)
}

// splitFlags pulls the named boolean flags out of args.
func splitFlags(args []string, names ...string) (flags map[string]bool, rest []string, err error) {
	flags = map[string]bool{}
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			if !known[arg] {
				return nil, nil, fmt.Errorf("unknown option %s; run mm help for usage", arg)
			}
			flags[arg] = true
			continue
		}
		rest = append(rest, arg)
	}
	return flags, rest, nil
}
