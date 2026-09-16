# hostdiff

**Why does it work on my other machine?** hostdiff compares two Macs or Linux
boxes side by side, and can fix the difference on either one.

You set up a new laptop, or a script works on one machine and fails on the
other, and the difference is buried somewhere: a Homebrew package, a missing
app, a keyboard shortcut, a Dock setting, a line in `.zshrc`, a different
`node` first on `PATH`.

- **Compare** this machine with another one over your existing ssh setup, with
  a saved snapshot, or two snapshot files.
- **Read** the differences as a table, one column per machine, with a content
  diff for any changed file.
- **Act**: install, update, remove, or make one machine like the other — after
  confirming the exact commands, which then run in your terminal.

It is a single Go binary for macOS and Linux. The other machine needs no
account, agent or service: hostdiff runs your own `ssh`, and if hostdiff is
not installed there it sends itself over for one run and deletes itself again.

## Contents

- [Install](#install)
- [Quick start](#quick-start)
- [The table](#the-table)
- [Changing things](#changing-things)
- [The other machine](#the-other-machine)
- [What it compares](#what-it-compares)
- [Printing instead of the table](#printing-instead-of-the-table)
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
hostdiff
```

That is the whole start. hostdiff asks four things in order:

1. **Which machine?** Machines from your config file, plus "something else…"
   where you can type an ssh destination (`me@host`), a snapshot file, or
   `laptop@last`. The first time, write the config with `hostdiff config init`
   and put a machine in it — or just type the destination here.
2. **Can it connect?** hostdiff tries the connection before collecting
   anything. If the host key is unknown or the login needs a password, it says
   so and offers `c`, which hands the terminal to `ssh` so you can check the
   fingerprint and type the password. That one connection is then reused for
   the rest of the run, so nothing asks again.
3. **What should be compared?** The groups: Homebrew, applications, keyboard
   shortcuts, language libraries, settings… `space` picks, `a` picks all,
   `enter` starts.
4. Both machines are scanned side by side, with progress per group, and the
   table appears.

To skip the questions: `hostdiff diff laptop` compares this machine with
`laptop` and scans everything; `--only brew,libraries` limits the groups.

No config and no ssh at all, using files:

```sh
hostdiff snap -o desk.json           # run on one machine
hostdiff snap -o laptop.json         # and on the other
hostdiff diff laptop.json desk.json
```

## The table

```text
◀ air (Mac, macOS/arm64)   ▶ mini (Mac-mini, macOS/arm64)
14 differences · 2 selected · enter: what to do with them
Homebrew            12 │                     air          mini
  formula            9 │ ◀ ● jq              1.7.1        —
  cask               3 │ ▶ ● rectangle       —            0.87
Language libraries 138 │ ≠   node            26.0.0       25.1.0
Dotfiles             2 │ =   git             2.50.1       2.50.1
```

- **Left:** the groups, with the number of differences. The selected group
  splits into its parts (formulae, casks, taps; each Python version, gems).
- **Right:** one row per item, one column per machine. `◀` only on the first
  machine, `▶` only on the second, `≠` on both but different, `=` the same
  (hidden until you press `a`). `—` means "not on this machine".
- `●` marks an item you selected; after a run, `✓`, `✗` or `?` says what
  happened to it.

| Key | |
|---|---|
| `↑` `↓` | move; on the left between groups, on the right between items |
| `tab` | switch between the group list and the items |
| `space` | select an item; on the left, the whole group |
| `A` / `ctrl+a` | select everything in this group / in every group; again to clear |
| `enter` | on the left: open the group. On an item: what can be done with it |
| `v` | details: the value on each side, and the diff of a changed file |
| `/` | filter, `a` also show items that are the same, `d` show package dependencies |
| `s` | the whole install script, `o` what the last run did |
| `c` | other groups, `m` other machine, `r` scan again |
| `esc` | one step back; `q` quit |

## Changing things

Select items with `space` (or just stand on one) and press `enter`. hostdiff
lists what it can do with them, on either machine:

```text
cask › bambu-studio

  ↑ Update on air to mini's version
  ↑ Update on mini to air's version
  ✗ Remove from air
› ✗ Remove from mini
```

✚ install, ↑ update, ✎ set a setting, ✗ remove, ↺ reset a setting to its
default, ⇄ clone. Choosing one opens the confirmation:

```text
hostdiff will run these 2 commands on mini (ssh mini), in this order,
exactly as written:

✗ Remove
    1  brew uninstall --cask bambu-studio
    2  brew uninstall jq

Remove: this deletes 2 items from mini.

# How: the commands go into a temporary script, copied to mini over ssh, run
# there with /bin/sh in this terminal (ssh -t), then deleted.
# Before each command the script prints it with its number; you can answer
# password prompts.
# A failed command does not stop the rest; Ctrl-C stops after the current one.
# Nothing else is run. Tab shows the full script.

enter or y: run · tab full script · esc cancel
```

`enter` (or `y`) runs them. You see each command and its result as it goes,
then press Enter to come back. hostdiff scans the affected groups again and
says what happened, for example `mini: 1 of 2 removed, 1 failed`. Every item
is marked in the table (`✓` `✗` `?`), and `o` shows each command with its exit
status. That summary stays until the next run.

**Clone.** `C` offers "Make air like mini" (or the other way round) for the
groups you compared: installs, updates and removals in one run, removals last.
If anything would be removed you have to type `yes`.

**What can be changed:** Homebrew formulae, casks and taps (an update installs
the newest version), App Store apps (`mas`), npm, pnpm, pipx, uv, cargo, gh
and dotnet global tools, VS Code, Cursor and VSCodium extensions, pip `--user`
packages per Python version, gems, CPAN modules, Composer, LuaRocks and Dart
packages (R and Julia: install and remove), versions from pyenv, rbenv,
rustup, asdf, mise and uv, and simple macOS settings (`defaults write` and
`defaults delete`).

**Files that only exist as files** — your own fonts — are copied between the
machines instead: hostdiff pipes the file over the same ssh connection, in
either direction, and can delete one from either machine. Fonts installed for
all users are only reported, because changing them needs an administrator.

**What is only reported:** packages installed system-wide for an interpreter,
apps without a Homebrew cask, launch agents, and dotfiles — hostdiff never
copies a dotfile, because snapshots hold them with secrets redacted.
Nothing is ever downgraded, and nothing is removed unless you chose a removal.

## The other machine

hostdiff does not handle logins. It runs your system `ssh` with a plain
destination, so users, keys, ports, jump hosts and agents all come from
`~/.ssh/config`, exactly as for `ssh laptop`. The config file only names
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

`hostdiff config init` writes that file, `hostdiff config path` prints where
it is, and `hostdiff machines --check` tries every machine in it. The file is
refused if other users can write to it, or if it holds a setting hostdiff does
not know (such as `skip` under a machine heading). Destinations that look like
options (`-oProxyCommand=…`) or contain spaces are rejected.

**Without the config file.** Anything can be named directly:
`hostdiff diff ssh:me@host`, `hostdiff diff box.local` (a name with `@`, `.`
or `:` is read as a destination), `hostdiff diff laptop.json`,
`hostdiff diff laptop@last`. In the interactive machine list the same goes
under "something else…".

**Host keys and passwords.** Interactively, hostdiff checks the connection
first and lets `ssh` ask its own questions in your terminal (see
[Quick start](#quick-start)). It then keeps that connection open for the rest
of the run — collecting, installing and reading results all reuse it — and
closes it when hostdiff exits. From the command line there is nothing to
answer, so hostdiff says what to do instead: `ssh laptop` once to accept a
host key, or `ssh-copy-id laptop` to install your key.

**hostdiff on the other machine** is not required. When it is missing and both
machines run the same operating system and architecture, `upload = "auto"`
streams this binary over the connection into a private temporary folder, runs
it once and deletes it (also if the run is interrupted). Otherwise install
hostdiff there, or compare snapshot files.

**Saved snapshots.** `--save` keeps a snapshot under
`~/.local/state/hostdiff/snapshots/NAME/`, and `NAME@last` (or `NAME@prev`)
refers to it later: `hostdiff diff laptop@last laptop` shows what changed on
the laptop since then.

**The same machine twice** is refused before anything is collected: two
snapshots of one machine differ only in timing. That covers `localhost` twice,
the machine marked `local = true`, and an ssh destination that `ssh -G`
resolves to one of this machine's own addresses for the same account. Another
account on the same machine is compared normally.

## What it compares

| Group | macOS | Linux | What is read |
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

`hostdiff sections` prints the same list. These names are what `--only` and
`--skip` take, and what the group picker shows by title.

## Printing instead of the table

Piped, or with `--format`, `--all`, `--details`, `--script` or `--install`,
hostdiff prints and exits:

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

Dotfiles  2 differ · 5 same
  ≠ ~/.zshrc      content 5f0c0a2e3d41 │ content 9b21e04c7a10
  ≠ ~/.gitconfig  content 5fb7728f1b95 │ content 7926bf107405

6 differences. Add --details to see changed file contents.
```

- `--format markdown` for a report, `--format json` for a script.
- `--script` prints a shell script that would bring the second machine's
  items to the first. hostdiff never runs it.
- `--install NAME` does run it, after listing the numbered commands and
  asking; `--yes` skips the question. It then scans those groups again and
  prints how they look now.

## Command reference

```text
hostdiff                   choose a machine and the groups, then the table
hostdiff diff [A] B        compare two machines or snapshots (A defaults to this machine)
  --only LIST              compare only these groups
  --skip LIST              do not collect these groups
  --all                    also list items that are the same, and dependencies
  --details                show content diffs for changed files and plists
  --format FORMAT          text, markdown or json (default: the table in a terminal)
  --script                 print a shell script, run on A, that brings over what B has
  --install NAME           run those commands on NAME (A or B), then scan again
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

`diff` exits 0 when nothing differs, 1 when something does, and 2 on error
(including when no group could be read on both sides), so it can guard
scripts: `hostdiff diff golden.json --only brew --format text`.

## Safety and privacy

hostdiff only reads, until you choose an action in the table or pass
`--install`. Then it runs exactly the commands it listed, after you confirmed
them — and for a clone that removes anything, after you typed `yes`.

- **Secrets are removed where they are read.** Every value passes one
  redaction step on the machine being collected, before it is written to a
  snapshot or sent over ssh: private key blocks, known token formats (GitHub,
  AWS, Slack, OpenAI, Anthropic, npm, Google, Stripe, JWTs), passwords in
  URLs, authorization headers, `user:password` pairs, and literal values given
  to names like `TOKEN` or `PASSWORD` (in `NAME=value`, `set -x NAME value`,
  `--password value`, git config keys and plist dictionaries). Redaction works
  on recognisable shapes, so review a snapshot before you share it. Files that
  hold credentials by design (`.netrc`, `.npmrc`, `.env`, cloud credential
  files) are not read at all, and private SSH keys are only listed by name.
- **Snapshots are still personal.** They contain host names, git identity, ssh
  host entries and dotfile contents. They are written with mode 600; keep them
  to yourself.
- **Snapshots from elsewhere are untrusted.** Terminal control characters are
  stripped from anything read from a file or another machine, snapshots larger
  than 256 MB are refused, and every name is validated and quoted before it
  can appear in a command.
- **No privacy dialogs.** macOS asks for permission when a program opens files
  in protected folders (Documents, Desktop, iCloud Drive, other apps' data).
  hostdiff never opens a path inside one of those folders or through a link
  into one, never opens `~/Library/Shortcuts` (it asks the `shortcuts` tool),
  reads app shortcuts only through `defaults` for the apps System Settings
  lists, and does not run the `/usr/bin` stubs that would open an "install
  developer tools" or "install Java" window.
- **The remote side is fixed.** Only validated group names are passed to the
  other machine, nothing from the config reaches its shell unchecked, and an
  installer script is copied over its own connection so the command line
  carries only a checked path.

## Limits

- **Shortcuts over ssh.** macOS only lets the logged-in session list
  Shortcuts; elsewhere the `shortcuts` tool lists nothing, so an empty list is
  reported as not compared (also on a Mac that really has none). Run
  `hostdiff snap -o FILE` in Terminal on that Mac and compare the file.
- **Login items** (System Settings › General › Login Items) need either
  Automation permission or root to read, and are not collected yet.
- **Settings** are a curated list, not every preference domain.
- **Updates to a given version** are only possible where the package manager
  takes one; Homebrew installs its newest version instead.
- The PATH comes from running your login shell, so a startup file that prints
  or waits for input can make that group slow (it is limited to 8 seconds).

## Development

```sh
go test ./...                       # unit, interactive mode and end-to-end tests
scripts/tui-smoke.sh ./hostdiff     # drives the real interactive mode in tmux
HOSTDIFF_DEBUG=1 hostdiff snap      # time per group
```

The end-to-end tests run the real binary against a throwaway home and system
root with stub tools and a fake `ssh`, so they never read the machine they run
on. The smoke test picks a machine and a group, installs on both sides,
removes, and clones, all against stubs.

## License

MIT
