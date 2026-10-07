# macro-master (`mm`)

Save and run macro commands from the terminal. Copy a command, run
`mm add <name>`, and run it later with `mm <name>`, or just `mm` for a picker.
Macros can be organised in folders (`mm web/deploy`), and libraries can be
plain folders or git repos that sync automatically, so a team can share
macros. See `macro-master-plan.md` for the full design.

## Build and test

```sh
make test                  # go vet + go test ./...
make build                 # bin/mm
make install               # copies bin/mm to ~/.local/bin (PREFIX=... to change)
```

Requires Go (stdlib only) and git. `fzf` gives the picker fuzzy search and
a preview; without it `mm` falls back to a numbered menu. The Homebrew formula
installs `fzf` automatically.

## Try it without touching your real setup

```sh
export MM_HOME=$(mktemp -d)    # everything mm stores goes here
bin/mm help
```

## Homebrew

`Formula/macro-master.rb` is the formula for the tap. Before publishing,
replace `OWNER` and `sha256` with the real values.
`scripts/brew-local.sh` installs from this working copy through a local tap
and runs `brew test`; `scripts/brew-local.sh uninstall` removes it again.

## License

MIT. See [LICENSE](LICENSE).
