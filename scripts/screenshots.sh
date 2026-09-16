#!/bin/sh
# Regenerates the README screenshots in docs/images/.
#
# Everything runs in a throwaway world: two pretend machines ("air" here,
# "mini" over a stand-in ssh) with a simulated Homebrew and a few fonts, homes
# under /tmp, no system state. hostdiff itself is the real binary, driven
# through tmux; each screen is captured with colours and rendered to SVG by
# scripts/ansi2svg. Needs: go, tmux.
#
#   scripts/screenshots.sh
set -eu
REPO=$(cd "$(dirname "$0")/.." && pwd)
OUT="$REPO/docs/images"
W=$(mktemp -d /tmp/hdshots.XXXXXX)
SOCK="$W/tmux.sock"
T="tmux -S $SOCK"
cleanup() { $T kill-server 2>/dev/null || true; rm -rf "$W"; }
trap cleanup EXIT
mkdir -p "$OUT" "$W/bin" "$W/sys"
(cd "$REPO" && go build -o "$W/bin/hostdiff" ./cmd/hostdiff && go build -o "$W/ansi2svg" ./scripts/ansi2svg)

machine() { # machine HOME FORMULAE CASKS FONTS...
	home=$1; shift
	mkdir -p "$home/.stubs" "$home/.fixture" "$home/Library/Fonts"
	printf '%s' "$1" >"$home/.fixture/formulae"
	printf '%s' "$2" >"$home/.fixture/casks"
	: >"$home/.fixture/taps"
	shift 2
	for f in "$@"; do printf 'font' >"$home/Library/Fonts/$f"; done
	cat >"$home/.stubs/brew" <<'BREW'
#!/bin/sh
F="$HOME/.fixture"
drop() { grep -v "^$2 " "$F/$1" >"$F/$1.tmp" || true; mv "$F/$1.tmp" "$F/$1"; }
case "$1 $2" in
"list --formula") cat "$F/formulae";;
"list --cask") cat "$F/casks";;
"leaves --installed-on-request") cut -d' ' -f1 "$F/formulae";;
"tap ") cat "$F/taps";;
"install --cask") echo "==> Installing Cask $3"; echo "$3 1.0.0" >>"$F/casks"; echo "🍺  $3 was successfully installed!";;
"uninstall --cask") drop casks "$3"; echo "==> Uninstalling Cask $3";;
"install "*) echo "==> Pouring $2"; echo "$2 1.0.0" >>"$F/formulae"; echo "🍺  $2 was successfully installed!";;
"uninstall "*) drop formulae "$2"; echo "Uninstalling $2...";;
"upgrade "*) echo "==> Upgrading $2";;
esac
BREW
	chmod 755 "$home/.stubs/brew"
}

machine "$W/air" "git 2.51.0
jq 1.8.1
node 24.8.0
python@3.13 3.13.7
ripgrep 14.1.1
wget 1.25.0
" "iterm2 3.5.14
rectangle 0.87
" "Inter.ttf" "JetBrainsMono-Regular.ttf"
machine "$W/remotes/mini" "fzf 0.65.1
git 2.51.0
htop 3.4.1
node 22.19.0
python@3.13 3.13.5
ripgrep 14.1.1
" "bambu-studio 2.2.1
iterm2 3.5.14
raycast 1.103.2
" "FiraCode-Regular.ttf" "Inter.ttf"
mkdir -p "$W/remotes/mini/.local/bin"
cp "$W/bin/hostdiff" "$W/remotes/mini/.local/bin/hostdiff"

cat >"$W/bin/ssh" <<'EOF2'
#!/bin/sh
if [ "$1" = "-G" ]; then printf 'user me\nhostname 2001:db8::10\nport 22\n'; exit 0; fi
while [ $# -gt 0 ]; do if [ "$1" = "--" ]; then shift; break; fi; shift; done
dest=$1; shift
home="$FAKE_SSH_ROOT/$dest"
exec env -i HOME="$home" PATH="$home/.stubs:/usr/bin:/bin" HOSTDIFF_SYSROOT="$FAKE_SYSROOT" HOSTDIFF_HOSTNAME="$dest" SSH_CONNECTION="sandbox 1 sandbox 22" TERM=xterm-256color /bin/sh -c "$*"
EOF2
chmod 755 "$W/bin/ssh"
printf '[machines.air]\nlocal = true\n\n[machines.mini]\nssh = "mini"\n' >"$W/config.toml"
chmod 600 "$W/config.toml"

COLS=118 ROWS=30
$T -f /dev/null new-session -d -s t -x $COLS -y $ROWS \
	"env -i HOME=$W/air PATH=$W/air/.stubs:/usr/bin:/bin TERM=xterm-256color HOSTDIFF_SSH=$W/bin/ssh HOSTDIFF_SYSROOT=$W/sys HOSTDIFF_HOSTNAME=air FAKE_SSH_ROOT=$W/remotes FAKE_SYSROOT=$W/sys HOSTDIFF_CONFIG=$W/config.toml $W/bin/hostdiff; sleep 60"
$T set -g status off

nap() { perl -e "select(undef,undef,undef,$1)"; }
keys() { for k in "$@"; do $T send-keys -t t "$k"; nap 0.25; done; }
wait_for() {
	for _ in $(seq 1 120); do
		$T capture-pane -p -t t | grep -qF -- "$1" && { nap 0.5; return 0; }
		nap 0.25
	done
	echo "screenshots: timed out waiting for: $1" >&2
	$T capture-pane -p -t t >&2
	exit 1
}
shot() { # shot NAME TITLE
	$T capture-pane -e -p -t t | "$W/ansi2svg" -title "$2" -cols $COLS >"$OUT/$1.svg"
	echo "  docs/images/$1.svg"
}

wait_for "Compare with:"
shot machines "hostdiff"
keys Enter
wait_for "What should be compared?"
keys j Space
# Fonts is the twentieth group; the cursor already moved one down.
i=0
while [ $i -lt 17 ]; do $T send-keys -t t j; i=$((i + 1)); done
nap 0.3
keys Space
wait_for "2 selected"
shot groups "hostdiff"
keys Enter
wait_for "formula"
shot table "hostdiff"
keys Tab Space Space Enter
wait_for "Install on mini"
shot menu "hostdiff"
keys Enter
wait_for "exactly as written"
shot confirm "hostdiff"
keys y
wait_for "Press Enter to return"
shot run "hostdiff: the commands running"
keys Enter
wait_for "last run"
shot after "hostdiff"
echo "screenshots: done"
