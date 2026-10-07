package hosts

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// keychainService names mm's entries in the macOS Keychain.
const keychainService = "macro-master"

// Tokens finds and stores API tokens.
type Tokens struct {
	AuthFile string // auth.json, used when there's no Keychain
	Getenv   func(string) string
	// Run runs a command with stdin and returns its stdout. Tests fake it.
	Run      func(stdin string, name string, args ...string) (string, error)
	Keychain bool // use the macOS Keychain
}

// NewTokens returns token storage for this platform.
func NewTokens(authFile string) *Tokens {
	return &Tokens{AuthFile: authFile, Getenv: os.Getenv, Run: runCmd, Keychain: runtime.GOOS == "darwin"}
}

func runCmd(stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	out, err := cmd.Output()
	return string(out), err
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

// EnvName is the environment variable checked first for host's token.
func EnvName(host string) string {
	return "MM_TOKEN_" + strings.ToUpper(nonAlnum.ReplaceAllString(host, "_"))
}

// Lookup finds a token for host, first match wins: environment variable,
// stored token, gh (GitHub only), then git's credential helper (HTTPS
// remotes only). An empty result means no token.
func (t *Tokens) Lookup(host, typ string, https bool) string {
	if v := t.Getenv(EnvName(host)); v != "" {
		return v
	}
	if v, _ := t.Stored(host); v != "" {
		return v
	}
	if typ == GitHub {
		if out, err := t.Run("", "gh", "auth", "token", "--hostname", host); err == nil {
			if v := strings.TrimSpace(out); v != "" {
				return v
			}
		}
	}
	if https {
		out, err := t.Run("protocol=https\nhost="+host+"\n\n", "git", "credential", "fill")
		if err == nil {
			for line := range strings.SplitSeq(out, "\n") {
				if v, ok := strings.CutPrefix(line, "password="); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// Stored returns the token saved with mm auth, if any.
func (t *Tokens) Stored(host string) (string, error) {
	if t.Keychain {
		out, err := t.Run("", "security", "find-generic-password", "-s", keychainService, "-a", host, "-w")
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(out), nil
	}
	m, err := t.readFile()
	return m[host], err
}

// Store saves a token for host.
func (t *Tokens) Store(host, token string) error {
	if strings.ContainsAny(token, " \t\r\n\"'\\") {
		return errors.New("that token has spaces or quotes in it; check you pasted it correctly")
	}
	if t.Keychain {
		// Through security's command mode, so the token never appears in a
		// process list.
		cmd := `add-generic-password -U -s ` + keychainService + ` -a "` + host + `" -l "mm token for ` + host + `" -w "` + token + `"` + "\n"
		_, err := t.Run(cmd, "security", "-i")
		return err
	}
	m, err := t.readFile()
	if err != nil {
		return err
	}
	m[host] = token
	return t.writeFile(m)
}

// Remove deletes a stored token.
func (t *Tokens) Remove(host string) error {
	if t.Keychain {
		_, err := t.Run("", "security", "delete-generic-password", "-s", keychainService, "-a", host)
		return err
	}
	m, err := t.readFile()
	if err != nil {
		return err
	}
	delete(m, host)
	return t.writeFile(m)
}

func (t *Tokens) readFile() (map[string]string, error) {
	m := map[string]string{}
	data, err := os.ReadFile(t.AuthFile)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}

func (t *Tokens) writeFile(m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.AuthFile + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.AuthFile)
}
