package compute

// InstallScript is what Settings › Compute has the operator run on a
// machine to lend it to fog:
//
//	curl -fsSL <fog>/compute/v1/install.sh | FOG_PAIR_CODE=<code> sh -s -- <fog>
//
// It fetches fog-node from that fog (checked against the digest fog
// publishes), makes sure ffmpeg is there, pairs with the code, and
// installs fog-node as a login service (launchd) or a systemd unit. The
// code is never on a command line, where any user of the machine could
// read it (/proc/<pid>/cmdline, ps) and pair first: it comes from
// FOG_PAIR_CODE, or is asked for on the terminal, and goes to fog-node
// join the same way.
const InstallScript = `#!/bin/sh
# fog-node installer: lends this machine's hardware to fog.
set -eu

FOG="${1:?usage: FOG_PAIR_CODE=CODE install.sh FOG_ADDRESS}"
FOG=$(printf '%s' "$FOG" | sed 's|/*$||')
# The pairing code: from the environment, else asked for. Never an
# argument, here or to anything this runs.
if [ "${2:-}" != "" ]; then
  echo "fog-node: don't put the pairing code on the command line (anyone on this machine can read it there); set FOG_PAIR_CODE instead" >&2
  exit 2
fi
FOG_PAIR_CODE="${FOG_PAIR_CODE:-}"
if [ -z "$FOG_PAIR_CODE" ]; then
  if [ -r /dev/tty ]; then
    printf 'Pairing code (from Settings > Compute): ' >/dev/tty
    IFS= read -r FOG_PAIR_CODE </dev/tty
  fi
fi
if [ -z "$FOG_PAIR_CODE" ]; then
  echo "fog-node: no pairing code: set FOG_PAIR_CODE" >&2
  exit 2
fi
export FOG_PAIR_CODE

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux) OS=linux ;;
  *) echo "fog-node: $(uname -s) is not supported" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "fog-node: $(uname -m) is not supported" >&2; exit 1 ;;
esac

if [ "$OS" = darwin ]; then
  DIR="$HOME/Library/Application Support/fog-node"
elif [ "$(id -u)" -eq 0 ]; then
  DIR=/var/lib/fog-node
else
  DIR="$HOME/.local/share/fog-node"
fi
BIN="$DIR/bin/fog-node"
mkdir -p "$DIR/bin"

echo "==> fetching fog-node for $OS-$ARCH from $FOG"
curl -fsSL "$FOG/compute/v1/fog-node/$OS-$ARCH" -o "$BIN.new"
WANT=$(curl -fsSL "$FOG/compute/v1/fog-node/$OS-$ARCH.sha256" | tr -d '[:space:]')
GOT=$( { shasum -a 256 "$BIN.new" 2>/dev/null || sha256sum "$BIN.new"; } | awk '{print $1}')
if [ "$GOT" != "$WANT" ]; then
  rm -f "$BIN.new"
  echo "fog-node: the download doesn't match fog's checksum" >&2
  exit 1
fi
chmod 755 "$BIN.new"
mv "$BIN.new" "$BIN"

if ! command -v ffmpeg >/dev/null 2>&1 && [ ! -x /opt/homebrew/bin/ffmpeg ] && [ ! -x /usr/local/bin/ffmpeg ]; then
  if [ "$OS" = darwin ] && command -v brew >/dev/null 2>&1; then
    echo "==> installing ffmpeg with Homebrew"
    brew install ffmpeg
  else
    echo "fog-node: no ffmpeg found; install it (on a Mac: brew install ffmpeg) and run fog-node probe" >&2
  fi
fi

echo "==> pairing"
"$BIN" join --dir "$DIR" --fog "$FOG"
unset FOG_PAIR_CODE
echo "==> installing the service"
"$BIN" install --dir "$DIR"
echo "done: this machine now shows in fog's Settings > Compute"
if [ "$OS" = darwin ]; then
  echo "macOS may ask whether fog-node may find devices on your local network: allow it."
  echo "fog-node picks the answer up by itself (System Settings > Privacy & Security > Local Network)."
fi
`
