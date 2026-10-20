// Package cli implements the mm commands.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"

	"macro-master/internal/clipboard"
	"macro-master/internal/hosts"
	"macro-master/internal/hotkey"
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
	// promptTTY is the terminal Prompts reads, so yes/no questions can take
	// a single key. Nil reads whole lines.
	promptTTY *os.File
	// Interactive is true when stdin and stdout are both terminals, which
	// the picker needs.
	Interactive bool
	// StyleOut and StyleErr turn on colours and panels for stdout and
	// stderr. Icons is "nerd" for Nerd Font glyphs; Width is the terminal's.
	StyleOut, StyleErr bool
	Icons              string
	Width              int
	// Height is the terminal's rows when stdout is one, otherwise 0.
	Height int

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
	// FetchLatest reads the newest mm version at an update source. Nil
	// asks the network; tests replace it.
	FetchLatest func(source string) (string, error)
	// Launchctl runs launchctl with args. Nil runs the real one.
	Launchctl func(args ...string) error
	// Notify shows a desktop notification. Nil shows a real one.
	Notify func(title, subtitle, body string, ok bool)

	store        *store.Store
	gitAvailable *bool
	// jobNotifyOff starts jobs with notifications off, for a quiet key
	// binding.
	jobNotifyOff bool
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
	if stdoutTTY {
		if _, h, err := term.GetSize(os.Stdout.Fd()); err == nil {
			a.Height = h
		}
	}
	if stdinTTY {
		a.Prompts, a.promptTTY = bufio.NewReader(os.Stdin), os.Stdin
	} else if tty, err := os.Open("/dev/tty"); err == nil {
		// stdin is busy (a pipe or --stdin content); ask on the terminal.
		a.Prompts, a.promptTTY = bufio.NewReader(tty), tty
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
	if len(args) > 0 && args[0] == hotkey.DaemonArg {
		return a.cmdHotkeyd()
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
	// Whatever changed config.json, the shell hook picks it up from here.
	defer func() {
		if err := a.store.SyncAliasFile(); err != nil {
			a.warnf("can't update the alias file for your shell: %v", err)
		}
	}()
	// A hotkey's run has no terminal to show notices on, and should start
	// its job without delay.
	quiet := len(args) >= 2 && args[0] == "sync" && args[1] == "--background" ||
		len(args) >= 1 && args[0] == "__bind"
	if !quiet {
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
		detach, name, rest, err := runFlags(rest)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return errors.New("mm run needs a macro name")
		}
		if detach {
			return a.cmdRunDetached(rest[0], rest[1:], name)
		}
		return a.cmdRun(rest[0], rest[1:])
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
	case "alias":
		return a.cmdAlias(rest)
	case "__alias":
		return a.cmdAliasRun(rest)
	case "bind":
		return a.cmdBind(rest)
	case "__bind":
		return a.cmdBindFire(rest)
	}
	if detach, name, rest, err := runFlags(args); err != nil || detach {
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return fmt.Errorf("%s needs a macro name: mm -d [-n <job name>] <macro>", cmd)
		}
		return a.cmdRunDetached(rest[0], rest[1:], name)
	}
	if strings.HasPrefix(cmd, "-") {
		return fmt.Errorf("unknown option %s; run mm help for usage", cmd)
	}
	return a.cmdRun(cmd, rest)
}

// runFlags reads the options before a macro name: -d/--detach, and
// -n/--name <job name>, which also detaches. Parsing stops at the first
// argument that isn't one, so the macro's own arguments pass through.
func runFlags(args []string) (detach bool, name string, rest []string, err error) {
	for len(args) > 0 {
		arg := args[0]
		switch {
		case arg == "-d" || arg == "--detach":
			detach, args = true, args[1:]
		case arg == "-n" || arg == "--name":
			if len(args) < 2 || args[1] == "" {
				return false, "", nil, fmt.Errorf("%s needs a job name: mm -d %s <job name> <macro>", arg, arg)
			}
			detach, name, args = true, args[1], args[2:]
		case strings.HasPrefix(arg, "--name="):
			detach, name, args = true, strings.TrimPrefix(arg, "--name="), args[1:]
			if name == "" {
				return false, "", nil, errors.New("--name= needs a job name: mm -d --name=<job name> <macro>")
			}
		default:
			return detach, name, args, nil
		}
	}
	return detach, name, nil, nil
}

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
	if a.promptTTY != nil {
		return a.askEdit(question)
	}
	a.printQuestion(question)
	return a.readAnswer()
}

// askPlain asks a question and reads the answer as the terminal sends it,
// with no editing, for answers that mustn't be echoed.
func (a *App) askPlain(question string) (string, error) {
	if a.Prompts == nil {
		return "", errors.New("this needs a terminal to answer questions")
	}
	a.printQuestion(question)
	return a.readAnswer()
}

// readAnswer reads a whole line of answer.
func (a *App) readAnswer() (string, error) {
	line, err := a.Prompts.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		fmt.Fprintln(a.Stderr)
		return "", errCancelled
	}
	return strings.TrimSpace(line), nil
}

func (a *App) printQuestion(question string) {
	if u := a.errUI(); u.on {
		// A trailing [default] or [y/N] hint is dimmed.
		q, hint := strings.TrimRight(question, " "), ""
		if i := strings.LastIndex(q, " ["); i >= 0 && (strings.HasSuffix(q, "]") || strings.HasSuffix(q, "]:")) {
			q, hint = q[:i], q[i:]
		}
		question = u.accent("? ") + u.bold(q) + u.faint(hint) + " "
	}
	fmt.Fprint(a.Stderr, question)
}

// confirm asks a yes/no question; def is the answer for a bare Enter.
func (a *App) confirm(question string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	if a.promptTTY != nil {
		return a.confirmKey(question+" "+hint+" ", def)
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

// confirmKey answers a yes/no question with one key on the terminal: y or
// n, enter for the default, ctrl+c, ctrl+d or esc to cancel.
func (a *App) confirmKey(question string, def bool) (bool, error) {
	a.printQuestion(question)
	state, err := term.MakeRaw(a.promptTTY.Fd())
	if err != nil {
		return false, err
	}
	var yes bool
	for {
		k, err := a.Prompts.ReadByte()
		if k == 0x1b && a.Prompts.Buffered() > 0 {
			// An arrow or other special key: ignored.
			a.Prompts.Discard(a.Prompts.Buffered())
			continue
		}
		if err != nil || k == 0x03 || k == 0x04 || k == 0x1b {
			term.Restore(a.promptTTY.Fd(), state)
			fmt.Fprintln(a.Stderr)
			if k == 0x03 {
				return false, exitCode(130)
			}
			return false, errCancelled
		}
		switch k {
		case 'y', 'Y':
			yes = true
		case 'n', 'N':
			yes = false
		case '\r', '\n':
			yes = def
		default:
			continue
		}
		break
	}
	term.Restore(a.promptTTY.Fd(), state)
	if yes {
		fmt.Fprintln(a.Stderr, "y")
	} else {
		fmt.Fprintln(a.Stderr, "n")
	}
	return yes, nil
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
