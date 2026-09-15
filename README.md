# hostdiff

**Why does it work on my other machine? hostdiff compares two Macs or Linux boxes side by side.**

You set up a new laptop, or a script works on one machine and fails on the
other, and the difference is buried somewhere: a Homebrew package, a missing
app, a keyboard shortcut, a Dock setting, a line in `.zshrc`, a different
`node` first on `PATH`. hostdiff collects all of that from both machines and
shows only what differs.

- **Compare** this machine with another one over your existing ssh setup, or two saved snapshots.
- **Browse** the differences section by section, with a content diff for any changed file.
- **Act on it** on either machine: install, update, remove or clone, after confirming the exact commands (over ssh for the other machine).

It is a single Go binary for macOS and Linux. The other machine does not need
hostdiff installed: when it is missing and the systems match, the binary is
sent over for one run and removed again.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [What it compares](#what-it-compares)
- [Machines and the config file](#machines-and-the-config-file)
- [Command reference](#command-reference)
- [Safety and privacy](#safety-and-privacy)
- [Limits](#limits)
- [Development](#development)
- [License](#license)

## Install

```sh
go install github.com/morass/hostdiff/cmd/hostdiff@latest
```

Or from a checkout: `go build -o ~/.local/bin/hostdiff ./cmd/hostdiff`.

## Quick start

```sh
hostdiff config init                 # creates ~/.config/hostdiff/config.toml
$EDITOR "$(hostdiff config path)"     # add a machine: ssh = "laptop"
hostdiff                             # pick the machine and what to compare
hostdiff diff laptop                 # this machine vs laptop, everything
```

No config at all is needed for files:

```sh
hostdiff snap -o desk.json           # on one machine
hostdiff snap -o laptop.json         # on the other
hostdiff diff laptop.json desk.json
```

In a terminal, `hostdiff` on its own asks which machine to compare with and
which groups (Homebrew, language libraries, settings, …), then scans both
machines with live progress and shows a table: one row per item, one column
per machine, ◀ only on the first, ▶ only on the second, ≠ different. Groups
split into their parts on the left (formulae and casks, each Python version
and gems). `hostdiff diff laptop` goes straight to the table; `--only`
chooses the groups. Piped or with `--format text|markdown|json` it prints.

**Acting on the results.** Select items with `space` (on the left: the whole
group), then `enter` lists what hostdiff can do with them on either machine:
install, update to the other machine's version, remove, or set and reset a
setting. Every choice opens a confirmation with the exact commands; `y` runs
them in your terminal, on this machine directly and on the other over ssh
with a terminal, so `sudo` and installer prompts work. Steps run one by one,
a failure does not stop the rest, Ctrl-C stops after the current step, and
the affected groups are scanned again. `C` (clone) makes one machine like
the other in the compared groups: installs, updates and removals together,
and it asks you to type `yes` when anything would be removed. Other keys:
`v` details and content diffs, `/` filter, `a` also same items, `d`
dependencies, `s` the install script, `c` other groups, `m` other machine,
`r` rescan.

Installing without the table: `hostdiff diff laptop --install localhost`
lists the commands that bring laptop's items here and asks (`--yes` skips
the question).

What hostdiff can install, update and remove: Homebrew formulae, casks and
taps (updates go to the newest version), App Store apps (`mas`), npm, pnpm,
pipx, uv, cargo, gh and dotnet global tools, VS Code, Cursor and VSCodium
extensions, pip `--user` packages per Python version, gems, CPAN modules,
Composer, LuaRocks and Dart packages (R and Julia: install and remove),
pyenv, rbenv, rustup, asdf, mise and uv Python versions, and scalar macOS
settings (`defaults write` and `defaults delete`). Packages installed
system-wide for an interpreter, apps without a cask and dotfiles are listed
but not changed (snapshots hold dotfiles with secrets redacted).

```text
◀ desk (desk, macOS/arm64, 2026-09-15 17:30)
▶ laptop (lap, macOS/arm64, 2026-09-15 17:30)

Homebrew  3 only on laptop · 1 only on desk · 2 differ · 12 in dependencies · 140 same
  ▶ cask › rectangle           0.87
  ▶ formula › jq               1.7.1
  ▶ formula › ripgrep          14.1.0
  ◀ formula › wget             1.25.0
  ≠ formula › node             25.1.0 │ 26.8.1
  ≠ formula › python@3.13      3.13.5 │ 3.13.7
  … and 12 differences in dependencies (installed only because other packages need them; --all lists them)

Keyboard shortcuts  1 only on laptop · 1 differ · 24 same
  ▶ app › com.apple.Safari › Show Tab Overview  cmd+shift+\
  ≠ system › Show Spotlight search             on cmd+space │ on ctrl+space

Dotfiles  2 differ · 5 same
  ≠ ~/.zshrc      content 5f0c0a2e3d41 │ content 9b21e04c7a10
  ≠ ~/.gitconfig  content 5fb7728f1b95 │ content 7926bf107405

6 differences. Add --details to see changed file contents.
```

## What it compares

| Section | macOS | Linux | What is read |
|---|---|---|---|
| `system` | ✓ | ✓ | OS version and build, architecture, Rosetta, Xcode and command line tools, login shell, time zone, printers |
| `brew` | ✓ | ✓ | Homebrew formulae and casks with versions, which were requested, taps |
| `mas` | ✓ | | App Store apps (with [mas](https://github.com/mas-cli/mas)) |
| `apps` | ✓ | | `/Applications` and `~/Applications` with versions |
| `syspkg` | | ✓ | manually installed apt packages, dnf, pacman, snap, flatpak |
| `shortcuts` | ✓ | | Shortcuts by name and folder |
| `keyboard` | ✓ | | system hotkeys, app menu shortcuts from System Settings, Services, input sources |
| `defaults` | ✓ | | a curated list of Dock, Finder, keyboard, trackpad, screenshot, clock and window settings |
| `launchd` | ✓ | | launch agents and daemons, launchctl enable/disable overrides |
| `systemd` | | ✓ | enabled system and user units |
| `shell` | ✓ | ✓ | shell versions, the PATH a login shell builds |
| `dotfiles` | ✓ | ✓ | shell, git, editor, terminal and window manager config files, compared by content |
| `git` | ✓ | ✓ | `git config --global` |
| `ssh` | ✓ | ✓ | `~/.ssh/config` host entries, public key fingerprints, which private keys exist |
| `runtimes` | ✓ | ✓ | versions and locations of about 50 runtimes and tools (node, python, go, java, docker, …) |
| `packages` | ✓ | ✓ | npm, pnpm and yarn globals, pipx, uv tools, cargo installs, go binaries, deno, dotnet tools, gh extensions |
| `libraries` | ✓ | ✓ | Python packages for every interpreter found (python3 and side-by-side python3.X, marked user or system), Ruby gems, Perl CPAN modules, Composer global, R packages, Julia environments, LuaRocks, Dart pub global |
| `toolchains` | ✓ | ✓ | versions installed by pyenv, rbenv, nvm, fnm, volta, rvm, asdf, mise, rustup (toolchains and components), Go SDKs, JDKs, SDKMAN, .NET SDKs and runtimes, conda environments, uv-managed Pythons |
| `editors` | ✓ | ✓ | VS Code, Cursor and VSCodium extensions, Vim, Neovim and tmux plugins |
| `fonts` | ✓ | ✓ | user and system fonts |
| `cron` | ✓ | ✓ | the user's crontab |
| `etc` | ✓ | ✓ | `/etc/hosts`, `/etc/paths`, `/etc/paths.d`, `/etc/resolver`, `/etc/shells` |
| `power` | ✓ | | `pmset` settings |

`hostdiff sections` prints the same list. `--only` and `--skip` choose sections.

## Machines and the config file

hostdiff does not handle logins. It runs your system `ssh` with a plain
destination, so users, keys, ports, jump hosts and agents all come from
`~/.ssh/config` exactly as for `ssh laptop`. The config file only names
machines:

```toml
# ~/.config/hostdiff/config.toml  (mode 600, never in a repository)

ignore = ["apps:Xcode*.app", "runtimes:docker"]   # SECTION:GLOB or GLOB
skip = ["fonts"]                                   # never collect these

# Machines come after the settings: in TOML every line after a
# [machines.NAME] heading belongs to that machine.
[machines.laptop]
ssh = "laptop"            # a Host alias from ~/.ssh/config, or user@host

[machines.server]
ssh = "me@server.example.org"
command = "~/.local/bin/hostdiff"   # where hostdiff lives there (default: search)
upload = "never"                    # auto (default), always or never

[machines.desk]
local = true              # this machine, by name
```

The config file is refused when other users can write to it or when it has a
setting hostdiff does not know (such as `skip` placed under a machine), and
`ssh` values that look like options (`-oProxyCommand=…`) or contain spaces are
rejected.

When hostdiff is not installed on the other machine and it runs the same
operating system and architecture, `upload = "auto"` streams this binary over
the ssh connection into a private temporary folder, runs it once and deletes
it. Otherwise install hostdiff there, or use snapshot files.

**Saved snapshots.** `--save` keeps a snapshot under
`~/.local/state/hostdiff/snapshots/NAME/`, and `NAME@last` (or `NAME@prev`)
refers to it later: `hostdiff diff laptop@last laptop` shows what changed on
the laptop since then.

**Same machine.** A live comparison of a machine with itself is refused
before anything is collected: `localhost` twice, the machine marked
`local = true`, or an ssh destination that `ssh -G` resolves to one of this
machine's addresses for the same account. Another account on the same machine
is still compared.

## Command reference

```text
hostdiff diff [A] B        compare two machines or snapshots (A defaults to this machine)
  --only LIST              compare only these sections
  --skip LIST              do not collect these sections
  --all                    also list items that are the same, and dependencies
  --details                show content diffs for changed files and plists
  --format FORMAT          text, markdown or json (default: interactive in a terminal)
  --script                 print a shell script, run on A, that brings over what B has
  --install NAME           install on NAME (A or B) what the other side has, then collect again
  --yes                    with --install: run without asking
  --save                   keep the collected snapshots for NAME@last

hostdiff snap [NAME]       take a snapshot of this or another machine
  -o FILE                  write it to FILE (mode 600)
  --json                   print it as JSON
  --save, --only, --skip   as above

hostdiff machines [--check]    list configured machines; --check tries each one
hostdiff config [init|path]    show or create the config file
hostdiff sections              what hostdiff collects
hostdiff help COMMAND          help for one command
```

`diff` exits 0 when nothing differs, 1 when something does and 2 on error
(including when no section could be read on both sides), so
it can guard scripts: `hostdiff diff golden.json --only brew --format text`.

## Safety and privacy

hostdiff only reads, unless you choose an action in the table or use
`--install`. Then it runs exactly the commands it showed you, after you
confirmed them (for a clone that removes anything, by typing `yes`).

- **Secrets are removed where they are read.** Every value passes one
  redaction step on the machine being collected, before it is written to a
  snapshot or sent over ssh: private key blocks, known token formats (GitHub,
  AWS, Slack, OpenAI, Anthropic, npm, Google, Stripe, JWTs), passwords in URLs,
  authorization headers, `user:password` pairs, and literal values given to
  names like `TOKEN` or `PASSWORD` (in `NAME=value`, `set -x NAME value`,
  `--password value`, git config keys and plist dictionaries). Redaction
  works on recognisable shapes, so review a snapshot before you share it. Files that hold credentials by design (`.netrc`, `.npmrc`,
  `.env`, cloud credential files) are not read at all, and private SSH keys
  are only listed by name.
- **Snapshots from elsewhere are untrusted.** Terminal control characters are
  removed from anything read from a file or another machine, and snapshots
  larger than 256 MB are refused.
- **Snapshots are still personal.** They contain host names, git identity,
  ssh host entries and dotfile contents. They are written with mode 600;
  keep them to yourself.
- **No privacy dialogs.** macOS asks for permission when a program opens files
  in protected folders (Documents, Desktop, iCloud Drive, other apps' data).
  hostdiff never opens a path that is in one of those folders or passes through
  a link into one, never opens `~/Library/Shortcuts` (it asks the `shortcuts`
  tool), reads app shortcuts only through `defaults` for the apps System
  Settings lists, and does not run the `/usr/bin` stubs that would open an
  "install developer tools" or "install Java" window.
- **The remote command is fixed.** Only validated section names are passed to
  the other machine; nothing from the config reaches its shell unchecked, and
  names coming back in a snapshot are validated and quoted before they appear
  in a generated script.

## Limits

- **Shortcuts over ssh.** macOS only lets the logged-in session list
  Shortcuts; elsewhere the `shortcuts` tool lists nothing, so an empty list
  is reported as not compared (also on a Mac that really has none). Run
  `hostdiff snap -o FILE` in Terminal on that Mac and compare the file.
- **Login items** (System Settings › General › Login Items) need either
  Automation permission or root to read and are not collected yet.
- **Settings** are a curated list, not every preference domain.
- The PATH comes from running your login shell, so a startup file that prints
  or waits for input can make that section slow (it is limited to 8 seconds).

## Development

```sh
go test ./...                       # unit, interactive mode and end-to-end tests
scripts/tui-smoke.sh ./hostdiff     # drives the real interactive mode in tmux
HOSTDIFF_DEBUG=1 hostdiff snap      # time per section
```

The end-to-end tests run the real binary against a throwaway home and system
root with stub tools and a fake `ssh`, so they never read the machine they run
on.

## License

MIT
