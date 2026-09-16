package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/morass/hostdiff/internal/collect"
)

type command struct {
	name, usage, summary, about, examples, flags string
}

var commands = []command{
	{
		name:    "diff",
		usage:   "hostdiff diff [A] B [flags]",
		summary: "compare two machines or snapshots",
		about: `Collects a snapshot of each side and prints what differs, section by
section: ◀ only on A, ▶ only on B, ≠ different on both. With one argument,
A is this machine. A and B can be:

  NAME          a machine from the config file (reached over ssh, or local)
  localhost     this machine (also "local" or ".")
  ssh:DEST      a machine that is not in the config file (user@host, or a
                Host alias from ~/.ssh/config); a name containing @ . or :
                is taken as a destination too
  FILE.json     a snapshot saved with "hostdiff snap -o FILE.json"
  NAME@last     the newest snapshot saved with --save for NAME (also @prev)

In a terminal, diff opens the interactive table: one row per item, one
column per machine. space selects items, enter lists what can be done with
them on either machine (install, update to the other version, remove, set
or reset a setting), and every action shows its commands for confirmation
before they run in the terminal. C makes one machine like the other. Run
hostdiff without a command to choose the machine and the groups first.
Piped, or with --format, --all or --details, it prints text.

The exit status of printed output is 0 when nothing differs, 1 when
something does, 2 on error.`,
		examples: `hostdiff diff laptop                  # this machine vs laptop
hostdiff diff laptop desk --only brew,apps
hostdiff diff laptop --details        # include changed file contents
hostdiff diff laptop --script > sync.sh   # commands to bring laptop's tools here
hostdiff diff laptop --install localhost --only brew   # install laptop's formulae here
hostdiff diff laptop --install laptop     # install this machine's extras on laptop
hostdiff diff laptop@last laptop      # what changed on laptop since the last --save
hostdiff diff old.json new.json --format markdown`,
		flags: `--only LIST       compare only these sections (comma separated)
--skip LIST       do not collect these sections
--all             also list items that are the same
--details         show content diffs for changed files and plists
--format FORMAT   text (default), markdown or json
--script          print a shell script, run on A, that brings over what B has (never runs it)
--install NAME    install on NAME (either side) what the other side has: prints the
                  commands, asks, runs them in this terminal, then collects again
--yes             with --install: do not ask
--save            also save the collected snapshots (see NAME@last)
--no-color        plain text output`,
	},
	{
		name:    "snap",
		usage:   "hostdiff snap [NAME] [flags]",
		summary: "take a snapshot of this or another machine",
		about: `Collects a snapshot of this machine, or of NAME from the config file. Use it
to compare later, or where a live connection is not possible: on a Mac, some
sections (Shortcuts) can only be read from a Terminal on that Mac, so run
"hostdiff snap -o FILE" there and compare the file.

Secrets are removed while collecting, on the machine being read. Snapshots
still contain personal details (host names, e-mail in git config, file
contents); keep them to yourself.`,
		examples: `hostdiff snap                         # summary of this machine
hostdiff snap -o desk.json            # save to a file
hostdiff snap laptop --save           # collect over ssh and keep it for NAME@last
hostdiff snap --only keyboard --json  # print one section as JSON`,
		flags: `-o FILE        write the snapshot to FILE (mode 600)
--json         write the snapshot as JSON to standard output
--save         keep it under the state directory for NAME@last
--only LIST    collect only these sections
--skip LIST    do not collect these sections`,
	},
	{
		name:    "machines",
		usage:   "hostdiff machines [--check]",
		summary: "list configured machines",
		about: `Lists the machines in the config file and how each is reached. With
--check, connects to each ssh machine and reports whether hostdiff is
installed there.`,
		examples: `hostdiff machines
hostdiff machines --check`,
		flags: `--check   try to reach each machine`,
	},
	{
		name:    "config",
		usage:   "hostdiff config [init|path]",
		summary: "show or create the config file",
		about: `The config file names machines and how to reach them. It holds no secrets:
logins, keys, ports and jump hosts stay in ~/.ssh/config, and hostdiff runs
"ssh DESTINATION". It lives in your config directory
($XDG_CONFIG_HOME/hostdiff/config.toml or ~/.config/hostdiff/config.toml;
$HOSTDIFF_CONFIG overrides), never in a repository.

  hostdiff config         print the path and the file
  hostdiff config init    create it with commented examples
  hostdiff config path    print only the path`,
		examples: `hostdiff config init
$EDITOR "$(hostdiff config path)"`,
	},
	{
		name:    "sections",
		usage:   "hostdiff sections",
		summary: "list what hostdiff collects",
		about:   `Lists every section with the operating system it applies to and what it reads.`,
	},
	{
		name:    "version",
		usage:   "hostdiff version",
		summary: "print the version",
	},
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func overview(w io.Writer) {
	fmt.Fprint(w, `hostdiff: why does it work on my other machine?

Compares two machines (or saved snapshots) side by side: Homebrew and App
Store apps, applications, Shortcuts, keyboard shortcuts, macOS settings,
launch agents, shell and PATH, dotfiles, git and ssh config, runtimes, global
packages, editor extensions, fonts and more. Other machines are reached with
your own ssh setup; secrets are removed before anything leaves a machine.

Usage:
  hostdiff COMMAND [flags]

Commands:
`)
	for _, c := range commands {
		fmt.Fprintf(w, "  %-10s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Start:
  hostdiff config init          # then add your machines
  hostdiff                      # choose a machine and what to compare
  hostdiff diff NAME            # this machine vs NAME

More: hostdiff help COMMAND, or hostdiff COMMAND --help
`)
}

func commandHelp(w io.Writer, c *command) {
	fmt.Fprintf(w, "%s\n\n  %s\n", c.summary, c.usage)
	if c.about != "" {
		fmt.Fprintf(w, "\n%s\n", c.about)
	}
	if c.name == "sections" {
		fmt.Fprintln(w)
		sectionsList(w)
	}
	if c.examples != "" {
		fmt.Fprintf(w, "\nExamples:\n%s\n", indent(c.examples))
	}
	if c.flags != "" {
		fmt.Fprintf(w, "\nFlags:\n%s\n", indent(c.flags))
	}
}

func sectionsList(w io.Writer) {
	for _, c := range collect.All() {
		os := "all"
		switch c.OS {
		case "darwin":
			os = "macOS"
		case "linux":
			os = "Linux"
		}
		fmt.Fprintf(w, "  %-10s %-6s %s\n", c.Kind, os, c.Reads)
	}
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
