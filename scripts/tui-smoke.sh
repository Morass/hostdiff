#!/bin/sh
# Drives the real interactive view in tmux against a throwaway world (stub
# brew, fake ssh): marks one item for each machine, installs, and checks
# the screen. Nothing on this machine is read or installed.
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

tmux -S "$sock" new-session -d -x 120 -y 30 \
	"env -i HOME=$w/home PATH=$w/home/.stubs:/usr/bin:/bin TERM=xterm-256color HOSTDIFF_SSH=$w/bin/ssh HOSTDIFF_SYSROOT=$w/sys FAKE_SSH_ROOT=$w/remotes FAKE_SYSROOT=$w/sys HOSTDIFF_CONFIG=$w/config.toml $bin diff laptop --only brew; sleep 30"

screen() { tmux -S "$sock" capture-pane -p; }
wait_for() {
	i=0
	until screen | grep -q -- "$1"; do
		i=$((i + 1))
		if [ $i -gt 100 ]; then
			echo "FAIL: waiting for: $1" >&2
			screen >&2
			exit 1
		fi
		sleep 0.1
	done
}
keys() { for k in "$@"; do tmux -S "$sock" send-keys "$k"; sleep 0.15; done; }

wait_for "formula › ripgrep"
keys Tab Space
wait_for "marked formula › wget to install on laptop"
keys j Space
wait_for "marked formula › ripgrep to install on localhost"
keys i
wait_for "brew install ripgrep"
keys y
wait_for "Press Enter to return"
wait_for "installed ripgrep"
keys Enter
wait_for "Press Enter to return"
wait_for "installed wget"
keys Enter
wait_for "laptop: 1 of 1 installed items now match"
# Both are installed now; the stub installs another version, so they may
# show as ≠ but never as only on one side.
if screen | grep -E -q "│ [◀▶] "; then
	echo "FAIL: an item is still missing after installing" >&2
	screen >&2
	exit 1
fi
grep -q "wget 9.9.9" "$w/remotes/laptop/.fixture/formulae"
grep -q "ripgrep 9.9.9" "$w/home/.fixture/formulae"
echo "ok: marked, installed on both machines, collected again"
