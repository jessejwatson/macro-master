package cli

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"macro-master/internal/gitx"
	"macro-master/internal/hosts"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

var shells = []string{"zsh", "bash", "fish"}

// initScripts define an mm shell function that lets "# mode: source"
// macros change the current shell. The binary writes such a macro to
// $MM_SOURCE_FILE instead of running it, and the function sources it.
var initScripts = map[string]string{
	"zsh":  posixInit("zsh"),
	"bash": posixInit("bash"),
	"fish": `# mm shell integration: add to ~/.config/fish/config.fish
#   mm init fish | source
function mm --description 'save and run macro commands'
    set -l __mm_src (mktemp -t mm-source.XXXXXX)
    or begin
        command mm $argv
        return $status
    end
    env MM_SOURCE_FILE=$__mm_src MM_SHELL=fish mm $argv
    set -l __mm_rc $status
    if test -s $__mm_src
        source $__mm_src
        set __mm_rc $status
    end
    command rm -f $__mm_src
    return $__mm_rc
end
`,
}

func posixInit(shell string) string {
	return `# mm shell integration: add to ~/.` + shell + `rc
#   eval "$(mm init ` + shell + `)"
mm() {
  local __mm_src __mm_rc
  __mm_src="$(mktemp "${TMPDIR:-/tmp}/mm-source.XXXXXX")" || { command mm "$@"; return; }
  MM_SOURCE_FILE="$__mm_src" MM_SHELL=` + shell + ` command mm "$@"
  __mm_rc=$?
  if [ -s "$__mm_src" ]; then
    . "$__mm_src"
    __mm_rc=$?
  fi
  command rm -f "$__mm_src"
  return $__mm_rc
}
`
}

var completionScripts = map[string]string{
	"zsh": `#compdef mm
# zsh completion for mm (macro-master)

_mm() {
  local -a cands spaced bare
  cands=("${(@f)$(command mm __complete "${(@)words[2,CURRENT]}" 2>/dev/null)}")
  bare=(${(M)cands:#*[=/]})
  spaced=(${cands:#*[=/]})
  (( ${#bare} )) && compadd -Q -S '' -- "${bare[@]}"
  (( ${#spaced} )) && compadd -Q -- "${spaced[@]}"
}

if [ "$funcstack[1]" = "_mm" ]; then
  _mm "$@"
else
  compdef _mm mm
fi
`,
	"bash": `# bash completion for mm (macro-master)

_mm() {
  local cur="${COMP_WORDS[COMP_CWORD]}" IFS=$'\n' c
  local -a cands
  cands=($(command mm __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null))
  COMPREPLY=()
  for c in "${cands[@]}"; do
    [[ "$c" == "$cur"* ]] && COMPREPLY+=("$c")
  done
  if [[ ${#COMPREPLY[@]} -eq 1 && "${COMPREPLY[0]}" == *[=/] ]] && type compopt >/dev/null 2>&1; then
    compopt -o nospace
  fi
}
complete -F _mm mm
`,
	"fish": `# fish completion for mm (macro-master)

function __mm_complete
    set -l tokens (commandline -opc) (commandline -ct)
    command mm __complete $tokens[2..-1] 2>/dev/null
end
complete -c mm -f -a '(__mm_complete)'
`,
}

func (a *App) cmdShellScript(kind string, args []string) error {
	scripts := initScripts
	if kind == "completion" {
		scripts = completionScripts
	}
	if len(args) != 1 || scripts[args[0]] == "" {
		return fmt.Errorf("usage: mm %s zsh|bash|fish", kind)
	}
	fmt.Fprint(a.Stdout, scripts[args[0]])
	return nil
}

// writeSourceFile hands a "# mode: source" macro to the shell function.
// Arguments become the positional parameters.
func writeSourceFile(path, shell string, m macro.Macro, args []string) error {
	var b strings.Builder
	if shell == "fish" {
		b.WriteString("set argv")
		for _, a := range args {
			b.WriteString(" " + fishQuote(a))
		}
	} else {
		b.WriteString("set --")
		for _, a := range args {
			b.WriteString(" " + shQuote(a))
		}
	}
	b.WriteString("\n" + strings.TrimRight(m.Body, "\n") + "\n")
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func fishQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

var subcommands = []string{
	"add", "edit", "show", "print", "rm", "mv", "rename", "ls", "fav", "lib",
	"sync", "auth", "trust", "init", "completion", "run", "help",
}

// addresses lists every way to name each id that resolves to just that
// id: the full path, and each shorter tail of it that is unique.
func addresses(ids []string) []string {
	count := map[string]int{}
	full := map[string]bool{}
	for _, id := range ids {
		full[id] = true
		segs := strings.Split(id, "/")
		for n := 1; n <= len(segs); n++ {
			count[strings.Join(segs[len(segs)-n:], "/")]++
		}
	}
	var out []string
	for _, id := range ids {
		segs := strings.Split(id, "/")
		for n := 1; n <= len(segs); n++ {
			addr := strings.Join(segs[len(segs)-n:], "/")
			if count[addr] == 1 || full[addr] {
				out = append(out, addr)
			}
		}
	}
	return out
}

// dirsOf adds every folder prefix ("a/", "a/b/") of each address.
func dirsOf(addrs []string) []string {
	var out []string
	for _, a := range addrs {
		for i := strings.Index(a, "/"); i >= 0; {
			out = append(out, a[:i+1])
			j := strings.Index(a[i+1:], "/")
			if j < 0 {
				break
			}
			i += j + 1
		}
	}
	return out
}

// complete answers "mm __complete <words...>", where the last word is the
// one being typed. It reads only local files: no sync, no network.
// Paths complete one folder at a time.
func complete(s *store.Store, words []string) []string {
	if len(words) == 0 {
		words = []string{""}
	}
	cur, prev := words[len(words)-1], words[:len(words)-1]
	var macroIDs, folderIDs []string
	for _, r := range s.AllMacros() {
		macroIDs = append(macroIDs, r.ID())
	}
	for _, f := range s.AllFolders() {
		folderIDs = append(folderIDs, f.ID())
	}
	macros := func() []string {
		addrs := addresses(macroIDs)
		return append(addrs, dirsOf(addrs)...)
	}
	folders := func() []string {
		var out []string
		for _, l := range s.Libraries() {
			out = append(out, l.Name+"/")
		}
		for _, a := range addresses(folderIDs) {
			out = append(out, a+"/")
		}
		return append(out, dirsOf(addresses(folderIDs))...)
	}
	libs := func() []string {
		var out []string
		for _, l := range s.Libraries() {
			out = append(out, l.Name)
		}
		return out
	}
	placeholders := func(addr string) []string {
		refs := s.Find(addr)
		if len(refs) != 1 {
			return nil
		}
		content, err := refs[0].Read()
		if err != nil {
			return nil
		}
		var out []string
		for _, p := range macro.Placeholders(macro.Parse(content).Body) {
			out = append(out, p.Name+"=")
		}
		return out
	}

	var cands []string
	if len(prev) == 0 {
		cands = append(slices.Clone(subcommands), macros()...)
	} else {
		switch cmd := prev[0]; cmd {
		case "add":
			cands = []string{"--stdin"}
			if len(prev) == 1 {
				cands = append(cands, folders()...)
			}
		case "edit", "show", "rm", "fav", "trust":
			if len(prev) == 1 {
				cands = macros()
			}
		case "mv", "rename":
			switch len(prev) {
			case 1:
				cands = append(macros(), folders()...)
			case 2:
				if cmd == "mv" {
					cands = folders()
				}
			}
		case "run", "print":
			if len(prev) == 1 {
				cands = macros()
			} else {
				cands = placeholders(prev[1])
			}
		case "ls":
			cands = append(append(libs(), "--fav"), folders()...)
		case "lib":
			switch {
			case len(prev) == 1:
				cands = []string{"ls", "add", "share", "rename", "rm", "default"}
			case len(prev) == 2 && (prev[1] == "rm" || prev[1] == "default" || prev[1] == "rename"):
				cands = libs()
			case len(prev) == 2 && prev[1] == "share":
				for _, l := range s.Libraries() {
					if !l.Synced {
						cands = append(cands, l.Name)
					}
				}
			}
		case "sync":
			for _, l := range s.Libraries() {
				if l.Synced {
					cands = append(cands, l.Name)
				}
			}
		case "init", "completion":
			if len(prev) == 1 {
				cands = shells
			}
		case "auth":
			if len(prev) == 1 {
				seen := map[string]bool{}
				for h := range s.Config.Hosts {
					seen[h] = true
				}
				for _, l := range s.Libraries() {
					if l.Synced {
						if r, err := hosts.ParseRemote(gitx.Repo{Dir: l.Path}.RemoteURL()); err == nil {
							seen[r.Host] = true
						}
					}
				}
				for h := range seen {
					cands = append(cands, h)
				}
			}
		default: // a macro name: offer its placeholders
			cands = placeholders(cmd)
		}
	}

	// Offer only the next path segment: "infra/" rather than every macro
	// under it.
	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		rest, ok := strings.CutPrefix(c, cur)
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 && i < len(rest)-1 {
			c = cur + rest[:i+1]
		}
		if c == cur && strings.HasSuffix(c, "/") {
			continue // already typed in full
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}
