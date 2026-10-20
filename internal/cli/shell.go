package cli

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"macro-master/internal/gitx"
	"macro-master/internal/hosts"
	"macro-master/internal/hotkey"
	"macro-master/internal/macro"
	"macro-master/internal/store"
)

var shells = []string{"zsh", "bash", "fish"}

// initScript is the shell hook for shell. It defines an mm shell function
// that lets "# mode: source" macros change the current shell: the binary
// writes such a macro to $MM_SOURCE_FILE instead of running it, and the
// function sources it. It also defines each alias from mm alias as a
// function that runs mm __alias, reloading them from aliasFile before a
// prompt whenever its first line changes. A name the shell already uses
// (an alias, function, builtin, or a command unless forced) is left alone
// and passed to mm in MM_ALIAS_SKIPPED, for mm alias ls.
func initScript(shell, aliasFile string) string {
	switch shell {
	case "zsh":
		return posixInit(shell) + `
# Aliases from mm alias, (re)defined before a prompt when they change.
__mm_alias_file=` + shQuote(aliasFile) + `
__mm_alias_load() {
  local gen n opt
  { read -r gen < "$__mm_alias_file"; } 2>/dev/null
  [[ "$gen" == "${__mm_alias_gen-}" ]] && return 0
  for n in ${=__mm_alias_names}; do
    unfunction "$n" 2>/dev/null
  done
  __mm_alias_gen=$gen __mm_alias_names= __mm_alias_skipped=
  [[ -n "$gen" ]] || return 0
  {
    read -r gen
    while read -r n opt; do
      [[ "$n" =~ '^[a-z][a-z0-9_-]*$' ]] || continue
      if (( $+aliases[$n] || $+functions[$n] || $+builtins[$n] || $+reswords[$n] )) ||
        { [[ "$opt" != force ]] && (( $+commands[$n] )); }; then
        __mm_alias_skipped+="$n "
        continue
      fi
      functions[$n]="mm __alias $n \"\$@\""
      (( $+functions[compdef] )) && compdef __mm_alias_complete "$n"
      __mm_alias_names+="$n "
    done
  } < "$__mm_alias_file" 2>/dev/null
  return 0
}
__mm_alias_complete() {
  local -a cands spaced bare
  cands=("${(@f)$(command mm __complete __alias "${(@)words[1,CURRENT]}" 2>/dev/null)}")
  bare=(${(M)cands:#*=})
  spaced=(${cands:#*=})
  (( ${#bare} )) && compadd -Q -S '' -- "${bare[@]}"
  (( ${#spaced} )) && compadd -Q -- "${spaced[@]}"
}
autoload -Uz add-zsh-hook && add-zsh-hook precmd __mm_alias_load
`
	case "bash":
		return posixInit(shell) + `
# Aliases from mm alias, (re)defined before a prompt when they change.
__mm_alias_file=` + shQuote(aliasFile) + `
__mm_alias_load() {
  local gen n opt
  { read -r gen < "$__mm_alias_file"; } 2>/dev/null
  [[ "$gen" == "${__mm_alias_gen-}" ]] && return 0
  for n in $__mm_alias_names; do
    unset -f "$n"
    complete -r "$n" 2>/dev/null
  done
  __mm_alias_gen=$gen __mm_alias_names= __mm_alias_skipped=
  [[ -n "$gen" ]] || return 0
  {
    read -r gen
    while read -r n opt; do
      [[ "$n" =~ ^[a-z][a-z0-9_-]*$ ]] || continue
      case "$(type -t "$n")" in
        "") ;;
        file) [[ "$opt" == force ]] || { __mm_alias_skipped+="$n "; continue; } ;;
        *) __mm_alias_skipped+="$n "; continue ;;
      esac
      eval "$n() { mm __alias $n \"\$@\"; }"
      complete -F __mm_alias_complete "$n"
      __mm_alias_names+="$n "
    done
  } < "$__mm_alias_file" 2>/dev/null
  return 0
}
__mm_alias_complete() {
  local cur="${COMP_WORDS[COMP_CWORD]}" IFS=$'\n' c
  local -a cands
  cands=($(command mm __complete __alias "${COMP_WORDS[@]:0:COMP_CWORD+1}" 2>/dev/null))
  COMPREPLY=()
  for c in "${cands[@]}"; do
    [[ "$c" == "$cur"* ]] && COMPREPLY+=("$c")
  done
  if [[ ${#COMPREPLY[@]} -eq 1 && "${COMPREPLY[0]}" == *= ]] && type compopt >/dev/null 2>&1; then
    compopt -o nospace
  fi
}
case ";${PROMPT_COMMAND-};" in
  *";__mm_alias_load;"*) ;;
  *) PROMPT_COMMAND="__mm_alias_load${PROMPT_COMMAND:+;$PROMPT_COMMAND}" ;;
esac
`
	case "fish":
		return `# mm shell integration: add to ~/.config/fish/config.fish
#   mm init fish | source
function mm --description 'save and run macro commands'
    set -l __mm_src (mktemp -t mm-source.XXXXXX)
    or begin
        command mm $argv
        return $status
    end
    env MM_SOURCE_FILE=$__mm_src MM_SHELL=fish MM_ALIAS_HOOK=1 MM_ALIAS_SKIPPED="$__mm_alias_skipped" mm $argv
    set -l __mm_rc $status
    if test -s $__mm_src
        source $__mm_src
        set __mm_rc $status
    end
    command rm -f $__mm_src
    return $__mm_rc
end

# Aliases from mm alias, (re)defined before a prompt when they change.
set -g __mm_alias_file ` + fishQuote(aliasFile) + `
function __mm_alias_load --on-event fish_prompt
    set -l gen ''
    if test -r $__mm_alias_file
        read gen < $__mm_alias_file
    end
    test "$gen" = "$__mm_alias_gen"; and return 0
    for n in $__mm_alias_names
        functions -e $n
        complete -e -c $n
    end
    set -g __mm_alias_gen $gen
    set -g __mm_alias_names
    set -g __mm_alias_skipped
    test -n "$gen"; or return 0
    set -l first 1
    while read -l n opt
        if test $first = 1
            set first 0
            continue
        end
        string match -qr '^[a-z][a-z0-9_-]*$' -- $n; or continue
        set -l t (type -t $n 2>/dev/null)
        if test -n "$t"; and not test "$t" = file -a "$opt" = force
            set -a __mm_alias_skipped $n
            continue
        end
        eval "function $n --description 'mm alias'; mm __alias $n \$argv; end"
        complete -c $n -f -a '(__mm_alias_complete)'
        set -a __mm_alias_names $n
    end < $__mm_alias_file
end
function __mm_alias_complete
    set -l tokens (commandline -opc) (commandline -ct)
    command mm __complete __alias $tokens 2>/dev/null
end
`
	}
	return ""
}

func posixInit(shell string) string {
	return `# mm shell integration: add to ~/.` + shell + `rc
#   eval "$(mm init ` + shell + `)"
mm() {
  local __mm_src __mm_rc
  __mm_src="$(mktemp "${TMPDIR:-/tmp}/mm-source.XXXXXX")" || { command mm "$@"; return; }
  MM_SOURCE_FILE="$__mm_src" MM_SHELL=` + shell + ` MM_ALIAS_HOOK=1 MM_ALIAS_SKIPPED="${__mm_alias_skipped-}" command mm "$@"
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
	if len(args) != 1 || !slices.Contains(shells, args[0]) {
		return fmt.Errorf("usage: mm %s zsh|bash|fish", kind)
	}
	if kind == "completion" {
		fmt.Fprint(a.Stdout, completionScripts[args[0]])
		return nil
	}
	home, err := a.homeDir()
	if err != nil {
		return err
	}
	// The shell reads the alias file at its first prompt, so make sure it's
	// there and current. Failing that only delays the aliases.
	if s, err := store.Open(home); err == nil {
		s.SyncAliasFile()
	}
	fmt.Fprint(a.Stdout, initScript(args[0], filepath.Join(home, "aliases")))
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
	"sync", "auth", "trust", "init", "completion", "run", "help", "alias", "bind",
}

// comboCands completes a key combination: after what's typed up to the
// last +, each modifier not used yet and then each key.
func comboCands(cur string) []string {
	head := ""
	if i := strings.LastIndex(cur, "+"); i >= 0 {
		head = cur[:i+1]
	}
	var out []string
	for _, m := range hotkey.Modifiers() {
		if !strings.Contains("+"+head, "+"+m+"+") {
			out = append(out, head+m+"+")
		}
	}
	if head != "" {
		for _, k := range hotkey.Keys() {
			out = append(out, head+k)
		}
	}
	return out
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
	aliasNames := func() []string { return slices.Sorted(maps.Keys(s.Config.Aliases)) }
	placeholders := func(addr string) []string {
		refs := s.Find(addr)
		if al, ok := s.Config.Aliases[addr]; ok && len(refs) == 0 {
			refs = s.Find(al.Macro)
		}
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
		cands = append(append(slices.Clone(subcommands), macros()...), aliasNames()...)
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
		case "jobs":
			if len(prev) == 1 {
				cands = []string{"--all", "--live", "clear"}
			}
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
		case "alias":
			switch {
			case len(prev) == 1:
				cands = []string{"ls", "add", "rm", "accept"}
			case len(prev) == 2 && prev[1] == "rm":
				cands = aliasNames()
			case len(prev) >= 2 && prev[1] == "accept":
				cands = []string{"--force"}
				for _, r := range s.AllMacros() {
					content, _ := r.Read()
					for _, l := range macro.Parse(content).Aliases {
						if f := strings.Fields(l); len(f) > 0 && s.Config.Aliases[f[0]].Macro == "" {
							cands = append(cands, f[0])
						}
					}
				}
			case len(prev) == 3 && prev[1] == "add":
				cands = macros()
			case len(prev) > 3 && prev[1] == "add":
				cands = append(placeholders(prev[3]), "--force")
			}
		case "bind":
			bound := func() []string { return slices.Sorted(maps.Keys(s.Config.Bindings)) }
			switch {
			case len(prev) == 1:
				cands = []string{"ls", "add", "rm", "service"}
			case len(prev) == 2 && prev[1] == "rm":
				cands = bound()
			case len(prev) == 2 && prev[1] == "service":
				cands = []string{"install", "uninstall", "status"}
			case len(prev) == 2 && prev[1] == "add":
				cands = comboCands(cur)
			case len(prev) == 3 && prev[1] == "add":
				cands = append(macros(), aliasNames()...)
			case len(prev) > 3 && prev[1] == "add":
				cands = append(placeholders(prev[3]), "--quiet")
			}
		case "__alias": // an alias's own completion: mm __alias <alias> ...
			if len(prev) > 1 {
				cands = placeholders(prev[1])
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
