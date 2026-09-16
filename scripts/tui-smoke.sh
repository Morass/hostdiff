#!/bin/sh
# Drives the real interactive mode in tmux against a throwaway world (stub
# brew, fake ssh): picks the machine and the group, installs on both
# machines, removes on the other, clones, and checks the screen after each
# step. Nothing on this machine is read or changed.
#
#   scripts/tui-smoke.sh path/to/hostdiff
set -eu

bin=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
w=$(mktemp -d "${TMPDIR:-/tmp}/hostdiff-smoke.XXXXXX")
sock="$w/tmux.sock"
trap 'tmux -S "$sock" kill-server 2>/dev/null || true; rm -rf "$w"' EXIT

machine() { # home formulae
	mkdir -p "$1/.stubs" "$1/.fixture"
	printf '%s' "$2" > "$1/.fixture/formulae"
	cat > "$1/.stubs/brew" <<'EOF'
#!/bin/sh
case "$1 $2" in
"list --formula") cat "$HOME/.fixture/formulae";;
"list --cask") ;;
"leaves --installed-on-request") cut -d' ' -f1 "$HOME/.fixture/formulae";;
"install "*) echo "$2 9.9.9" >> "$HOME/.fixture/formulae"; echo "installed $2";;
"uninstall "*) grep -v "^$2 " "$HOME/.fixture/formulae" > "$HOME/.fixture/f.tmp"; mv "$HOME/.fixture/f.tmp" "$HOME/.fixture/formulae"; echo "uninstalled $2";;
"upgrade "*) echo "upgraded $2";;
esac
EOF
	chmod 755 "$1/.stubs/brew"
}

machine "$w/home" "jq 1.7.1
wget 1.25.0
"
machine "$w/remotes/laptop" "jq 1.7.1
ripgrep 14.1.0
"
mkdir -p "$w/remotes/laptop/.local/bin" "$w/bin" "$w/sys"
cp "$bin" "$w/remotes/laptop/.local/bin/hostdiff"
cat > "$w/bin/ssh" <<'EOF'
#!/bin/sh
if [ "$1" = "-G" ]; then printf 'user x\nhostname 2001:db8::10\nport 22\n'; exit 0; fi
while [ $# -gt 0 ]; do if [ "$1" = "--" ]; then shift; break; fi; shift; done
dest=$1; shift
home="$FAKE_SSH_ROOT/$dest"
exec env -i HOME="$home" PATH="$home/.stubs:/usr/bin:/bin" HOSTDIFF_SYSROOT="$FAKE_SYSROOT" SSH_CONNECTION="sandbox 1 sandbox 22" TERM=xterm /bin/sh -c "$*"
EOF
chmod 755 "$w/bin/ssh"
printf '[machines.laptop]\nssh = "laptop"\n' > "$w/config.toml"
chmod 600 "$w/config.toml"

tmux -S "$sock" new-session -d -x 130 -y 32 \
	"env -i HOME=$w/home PATH=$w/home/.stubs:/usr/bin:/bin TERM=xterm-256color HOSTDIFF_SSH=$w/bin/ssh HOSTDIFF_SYSROOT=$w/sys FAKE_SSH_ROOT=$w/remotes FAKE_SYSROOT=$w/sys HOSTDIFF_CONFIG=$w/config.toml $bin; sleep 30"

screen() { tmux -S "$sock" capture-pane -p; }
wait_for() {
	i=0
	until screen | grep -q -- "$1"; do
		i=$((i + 1))
		if [ $i -gt 150 ]; then
			echo "FAIL: waiting for: $1" >&2
			screen >&2
			exit 1
		fi
		sleep 0.1
	done
}
keys() { for k in "$@"; do tmux -S "$sock" send-keys "$k"; sleep 0.2; done; }
finish() { # wait for the installer, return to the view
	wait_for "Press Enter to return"
	keys Enter
}

# Machine, then group.
wait_for "Compare with:"
keys Enter
wait_for "What should be compared?"
keys j Space Enter
wait_for "formula › ripgrep"

# ◀ wget is only here: install it on laptop (first choice).
keys Tab Enter
wait_for "Install on laptop"
keys Enter
wait_for "brew install wget"
keys y
finish
wait_for "laptop: 1 of 1 installed"

# ▶ ripgrep is only on laptop: install it here.
keys Enter
wait_for "Install on localhost"
keys Enter y
finish
wait_for "localhost: 1 of 1 installed"

# Both now differ in version. Remove ripgrep from laptop (fourth choice).
keys Enter
wait_for "Remove from laptop"
keys j j j Enter
wait_for "brew uninstall ripgrep"
keys y
finish
wait_for "laptop: 1 of 1 removed"

# Clone: make localhost like laptop, which removes ripgrep here.
keys C
wait_for "Make localhost like laptop"
keys Enter
wait_for "type yes"
keys y e s Enter
finish
wait_for "of 2 applied"

grep -q "wget 9.9.9" "$w/remotes/laptop/.fixture/formulae"
! grep -q "ripgrep" "$w/remotes/laptop/.fixture/formulae"
! grep -q "ripgrep" "$w/home/.fixture/formulae"
echo "ok: picked machine and group, installed on both, removed, cloned"
