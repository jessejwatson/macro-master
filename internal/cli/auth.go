package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"macro-master/internal/gitx"
	"macro-master/internal/hosts"
	"macro-master/internal/store"
)

func (a *App) cmdAuth(args []string) error {
	flags, args, err := splitFlags(args, "--remove")
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: mm auth <host> [--remove], e.g. mm auth github.com")
	}
	host := strings.ToLower(args[0])
	if strings.ContainsAny(host, "/: ") {
		return fmt.Errorf("give just the host name, like github.com, not %q", args[0])
	}
	tk := a.tokens()
	where := "auth.json"
	if tk.Keychain {
		where = "the macOS Keychain"
	}

	if flags["--remove"] {
		if err := tk.Remove(host); err != nil {
			return fmt.Errorf("no token for %s was stored", host)
		}
		fmt.Fprintf(a.Stderr, "Removed the token for %s from %s.\n", host, where)
		a.recheckHost(host)
		return nil
	}

	if a.Prompts == nil {
		return errors.New("mm auth needs a terminal to read the token")
	}
	typ, api := a.hostType(host)
	fmt.Fprintf(a.Stderr, "Paste a token for %s (%s). It only needs read access to your repos.\n", host, typ)
	token, err := a.readSecret("Token: ")
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("no token entered; nothing saved")
	}
	if typ == hosts.Generic {
		fmt.Fprintf(a.Stderr, "mm doesn't know %s's API, so it can't check the token or permissions; saving it anyway.\n", host)
	} else if err := a.hostsClient().TestToken(typ, api, token); errors.Is(err, hosts.ErrBadToken) {
		return fmt.Errorf("%s rejected that token; nothing saved", host)
	} else if err != nil {
		fmt.Fprintf(a.Stderr, "Couldn't check the token (%v); saving it anyway.\n", err)
	}
	if err := tk.Store(host, token); err != nil {
		return fmt.Errorf("couldn't save the token: %w", err)
	}
	fmt.Fprintf(a.Stderr, "Saved the token for %s in %s.\n", host, where)
	a.recheckHost(host)
	return nil
}

// recheckHost forces a permission check on the next sync for libraries on
// host.
func (a *App) recheckHost(host string) {
	var names []string
	for _, l := range a.syncedLibraries() {
		if r, err := hosts.ParseRemote(gitx.Repo{Dir: l.Path}.RemoteURL()); err == nil && strings.EqualFold(r.Host, host) {
			names = append(names, l.Name)
		}
	}
	if len(names) == 0 {
		return
	}
	a.store.UpdateState(func(st *store.State) {
		for _, n := range names {
			st.Lib(n).LastPermCheck = time.Time{}
		}
	})
	fmt.Fprintf(a.Stderr, "Run mm sync to recheck permissions for %s.\n", strings.Join(names, ", "))
}

// readSecret reads a line without echoing it, using stty on the terminal.
func (a *App) readSecret(prompt string) (string, error) {
	if a.ReadSecret != nil {
		return a.ReadSecret(prompt)
	}
	if tty, err := os.Open("/dev/tty"); err == nil {
		defer tty.Close()
		off := exec.Command("stty", "-echo")
		off.Stdin = tty
		if off.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = tty
				on.Run()
				fmt.Fprintln(a.Stderr)
			}()
		}
	}
	return a.ask(prompt)
}
