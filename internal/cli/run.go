package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"macro-master/internal/macro"
	"macro-master/internal/store"
)

func (a *App) cmdRun(addr string, args []string) error {
	ref, err := a.resolve(addr)
	if err != nil {
		return err
	}
	return a.runMacro(ref, args)
}

func (a *App) cmdPrint(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mm print <name> [placeholder=value...]")
	}
	ref, err := a.resolve(args[0])
	if err != nil {
		return err
	}
	m, rest, _, err := a.prepare(ref, args[1:])
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("mm print only takes placeholder=value arguments, not %q", rest[0])
	}
	fmt.Fprintln(a.Stdout, strings.Trim(m.Body, "\n"))
	return nil
}

// prepare reads a macro, checks a shared one is trusted, and fills its
// placeholders from name=value args, then by prompting. It returns the
// filled macro, the args to pass on, and the file content as checked.
func (a *App) prepare(ref store.Ref, args []string) (macro.Macro, []string, string, error) {
	content, err := ref.Read()
	if err != nil {
		return macro.Macro{}, nil, "", err
	}
	if err := a.checkTrust(ref, content); err != nil {
		return macro.Macro{}, nil, "", err
	}
	m := macro.Parse(content)
	ph := macro.Placeholders(m.Body)
	values, rest := macro.SplitArgs(args, ph)
	for _, p := range ph {
		if _, ok := values[p.Name]; ok {
			continue
		}
		if a.Prompts == nil {
			if !p.HasDefault {
				return macro.Macro{}, nil, "", fmt.Errorf("{{%s}} needs a value; pass %s=<value> after the macro name", p.Name, p.Name)
			}
			values[p.Name] = p.Default
			continue
		}
		q := p.Name + ": "
		if p.HasDefault {
			q = fmt.Sprintf("%s [%s]: ", p.Name, p.Default)
		}
		ans, err := a.ask(q)
		if err != nil {
			return macro.Macro{}, nil, "", err
		}
		if ans == "" && p.HasDefault {
			ans = p.Default
		}
		values[p.Name] = ans
	}
	m.Body = macro.Fill(m.Body, values)
	return m, rest, content, nil
}

// runMacro runs the macro in a child process with the terminal attached and
// returns its exit status as an exitCode.
func (a *App) runMacro(ref store.Ref, args []string) error {
	m, rest, content, err := a.prepare(ref, args)
	if err != nil {
		return err
	}

	if m.Mode == "source" {
		if f := a.Getenv("MM_SOURCE_FILE"); f != "" {
			a.recordRun(ref)
			return writeSourceFile(f, a.Getenv("MM_SHELL"), m, rest)
		}
		a.notef("%s is meant to change your current shell, which needs the shell hook: add eval \"$(mm init zsh)\" to ~/.zshrc (or the bash/fish equivalent). Running it normally.", ref.ID())
	}

	// Run the saved file, or a copy when placeholders were filled or the
	// library is shared, so what runs is exactly what was checked, even if
	// a background sync changes the file meanwhile.
	script := ref.Path
	lib, _ := a.store.Library(ref.Library)
	if m.Body != macro.Parse(content).Body || lib.Synced {
		tmp, err := os.CreateTemp("", "mm-"+ref.Leaf()+"-*"+macro.Ext)
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, werr := tmp.WriteString(m.String())
		if cerr := tmp.Close(); werr != nil || cerr != nil {
			return errors.Join(werr, cerr)
		}
		script = tmp.Name()
	}

	argv := append(m.Interpreter(), script)
	argv = append(argv, rest...)
	path, err := a.lookPath(argv[0])
	if err != nil {
		return fmt.Errorf("can't find %s to run %s; check the macro's #! line", argv[0], ref.ID())
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	cmd.Env = childEnv("MM_MACRO="+ref.Name, "MM_LIBRARY="+ref.Library)

	a.recordRun(ref)

	// Ctrl-C and Ctrl-\ reach the child straight from the terminal, so mm
	// just survives them and waits. Signals sent to mm alone are forwarded.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("can't start %s: %w", ref.ID(), err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM || s == syscall.SIGHUP {
					cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	return exitStatus(err)
}

// exitStatus maps a child's result to mm's exit code, using 128+n for a
// child killed by signal n, as shells do.
func exitStatus(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return err
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return exitCode(128 + int(ws.Signal()))
	}
	return exitCode(ee.ExitCode())
}
