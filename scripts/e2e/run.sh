#!/bin/bash
# End-to-end benchmark: the current tree against a reference revision, each
# with its own disposable "VPS" container behind an emulated WAN delay.
#   scripts/e2e/run.sh [DELAY_MS] [REFERENCE_REVISION]
set -euo pipefail
# Running inside a remote-claude session must not make the clients under test
# act as that session's worker.
unset _REMOTE_CLAUDE_SERVER_ATTACH_WORKER _REMOTE_CLAUDE_SERVER_RETRY_MARKER _REMOTE_CLAUDE_SESSION_RETRY_NUMBER _REMOTE_CLAUDE_SERVER_REATTACH_ONLY _REMOTE_CLAUDE_ORDINARY_CONTINUE
DELAY=${1:-80}
REF=${2:-}
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
W=$(mktemp -d /tmp/rc-e2e.XXXXXX)
trap '[ -n "${KEEP:-}" ] || docker rm -f rc-e2e-new rc-e2e-old >/dev/null 2>&1 || true' EXIT
echo "work dir: $W" >&2

build() { (cd "$1" && CGO_ENABLED=0 go build -o "$2" .); }
mkdir -p "$W/new" "$W/old" "$W/keys"
build "$REPO" "$W/new/remote-claude"
variants=(new)
if [ -n "$REF" ]; then
  git -C "$REPO" worktree add -q --detach "$W/old-src" "$REF"
  build "$W/old-src" "$W/old/remote-claude"
  git -C "$REPO" worktree remove --force "$W/old-src"
  variants=(old new)
fi
ssh-keygen -q -t ed25519 -N '' -f "$W/keys/host" -C e2e-host
ssh-keygen -q -t ed25519 -N '' -f "$W/keys/client" -C e2e-client
FP=$(ssh-keygen -lf "$W/keys/host.pub" -E sha256 | awk '{print $2}')
docker build --network host -q -t remote-claude-e2e "$HERE" >/dev/null

# The project: sources, an ignored dependency tree and a Git history.
P0="$W/project"
mkdir -p "$P0/src" "$P0/node_modules/pkg"
for i in $(seq 1 100); do
  { echo "package src"; echo "// file $i"; for j in $(seq 1 40); do echo "func F${i}_$j() int { return $j }"; done; } > "$P0/src/f$i.go"
done
echo "// needle" >> "$P0/src/f7.go"
for i in $(seq 1 300); do echo "module.exports = $i // needle" > "$P0/node_modules/pkg/m$i.js"; done
echo "module.exports = 1" > "$P0/node_modules/pkg/index.js"
printf 'node_modules/\n*.log\n' > "$P0/.gitignore"
# E2E_PLAIN=1 checks a directory outside Git, which is not copied.
if [ -z "${E2E_PLAIN:-}" ]; then
  git -C "$P0" init -q -b main
  git -C "$P0" -c user.name=e2e -c user.email=e2e@example.com add -A
  git -C "$P0" -c user.name=e2e -c user.email=e2e@example.com commit -qm initial
fi

port=2221
for v in "${variants[@]}"; do
  port=$((port + 1))
  docker rm -f "rc-e2e-$v" >/dev/null 2>&1 || true
  docker run -d --name "rc-e2e-$v" --device /dev/fuse --cap-add SYS_ADMIN --cap-add NET_ADMIN \
    --security-opt apparmor:unconfined -p "127.0.0.1:$port:22" \
    -v "$W/$v/remote-claude:/opt/remote-claude/remote-claude:ro" \
    -v "$HERE/fake-claude.sh:/opt/remote-claude/claude:ro" \
    -v "$W/keys/host:/etc/ssh/e2e_host_ed25519_key:ro" \
    remote-claude-e2e >/dev/null
  docker exec "rc-e2e-$v" usermod -p '*' developer
  docker exec -i "rc-e2e-$v" bash -c "install -d -o developer -m 700 /home/developer/.ssh && install -o developer -m 600 /dev/stdin /home/developer/.ssh/authorized_keys" < "$W/keys/client.pub"
  docker exec "rc-e2e-$v" tc qdisc add dev eth0 root netem delay "${DELAY}ms"
  mkdir -p "$W/cfg-$v/remote-claude/default"
  printf '{"host":"127.0.0.1:%s","user":"developer","fingerprint":"%s","identity":"%s"}\n' "$port" "$FP" "$W/keys/client" > "$W/cfg-$v/remote-claude/default/config.json"
  cp -a "$P0" "$W/project-$v"
done
sleep 1

for v in "${variants[@]}"; do
  for run in first second; do
    s=$(date +%s%N)
    out=$(REMOTE_CLAUDE_STARTUP_TRACE=1 XDG_CONFIG_HOME="$W/cfg-$v" "$W/$v/remote-claude" --print go "$W/project-$v" 2>"$W/$v-$run.err" | tail -1) || { cat "$W/$v-$run.err" >&2; exit 1; }
    total=$(( ($(date +%s%N) - s) / 1000000 ))
    echo "$v $run total_ms=$total $out"; grep -h Startup "$W/$v-$run.err" | tr "\n" " "; echo
  done
done

if [ -z "$REF" ] || [ "${INTERACTIVE:-1}" = 1 ]; then
  python3 "$HERE/interactive.py" "$W/new/remote-claude" "$W/cfg-new" "$W/project-new" rc-e2e-new
fi
