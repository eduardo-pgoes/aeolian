#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="${AEOLIAN_BIN:-$HOME/.local/bin/aeolian}"
unit="${AEOLIAN_UNIT:-aeolian-host}"
addr="${AEOLIAN_ADDR:-127.0.0.1:7431}"

usage() {
	echo "usage: scripts/redeploy.sh [--test] [--rollback]"
	echo "  builds cmd/aeolian into $bin, keeps the old one as $bin.prev, restarts $unit"
	echo "  --test      run go test ./... before installing"
	echo "  --rollback  put $bin.prev back and restart"
}

test=0
rollback=0
for arg in "$@"; do
	case "$arg" in
	--test) test=1 ;;
	--rollback) rollback=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done

restart() {
	systemctl --user restart "$unit"
	for _ in $(seq 1 50); do
		if curl -sf -m 1 -o /dev/null "http://$addr/api/runs"; then
			echo "$unit is up on $addr"
			return 0
		fi
		sleep 0.2
	done
	echo "$unit did not answer on $addr; journalctl --user -u $unit -n 30:" >&2
	journalctl --user -u "$unit" -n 30 --no-pager >&2
	return 1
}

if ((rollback)); then
	[[ -x "$bin.prev" ]] || { echo "no $bin.prev to roll back to" >&2; exit 1; }
	mv -f "$bin.prev" "$bin"
	restart
	exit
fi

cd "$repo"
if ((test)); then
	go test ./... -count=1
fi

rev="$(git rev-parse --short HEAD)"
[[ -z "$(git status --porcelain -- cmd internal go.mod go.sum)" ]] || rev="$rev-dirty"

tmp="$(mktemp "$bin.new.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
go build -o "$tmp" ./cmd/aeolian
chmod +x "$tmp"

[[ -e "$bin" ]] && cp -f "$bin" "$bin.prev"
mv -f "$tmp" "$bin"
trap - EXIT
echo "installed $rev at $bin"

restart
