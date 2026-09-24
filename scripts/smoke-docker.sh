#!/usr/bin/env bash
# Exercise the actual Compose configuration with a synthetic, empty data directory.
# No GitHub accounts are loaded and no paid inference or external login is performed.
# Requirements: Docker with Compose v2+, bash, Python 3 and curl.
# Usage: bash scripts/smoke-docker.sh IMAGE [linux/amd64|linux/arm64]
set -euo pipefail
# A broken health endpoint must not hang a CI runner indefinitely.
curl() { command curl --connect-timeout 3 --max-time 15 "$@"; }
cd "$(dirname "$0")/.."
image="${1:-copilot-proxy:smoke}"
platform="${2:-}"
case "$platform" in
  ""|linux/amd64|linux/arm64) ;;
  *) printf "Unsupported smoke platform: %s\n" "$platform" >&2; exit 2 ;;
esac
tmp="$(mktemp -d)"
project="copilot-smoke-$$"
export COPILOT_IMAGE="$image"
export COPILOT_DATA_DIR="$tmp/data"
COPILOT_UID="$(id -u)"
COPILOT_GID="$(id -g)"
export COPILOT_UID COPILOT_GID
export COPILOT_BIND_ADDRESS=127.0.0.1 COPILOT_PORT=0
# The override avoids pulling an image that was just built locally in CI.
printf 'services:\n  copilot-proxy:\n    pull_policy: never\n' > "$tmp/override.yaml"
if [[ -n "$platform" ]]; then
  printf '    platform: %s\n' "$platform" >> "$tmp/override.yaml"
fi
compose=(docker compose --env-file /dev/null -p "$project" -f compose.yaml -f "$tmp/override.yaml")
cleanup() {
  "${compose[@]}" down --timeout 10 >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT
python3 - "$tmp/data" <<'PY'
import os, pathlib, secrets, sys
root = pathlib.Path(sys.argv[1])
root.mkdir(mode=0o700)
text = pathlib.Path('config.example.yaml').read_text()
text = text.replace('REPLACE_ADMIN', secrets.token_hex(32))
text = text.replace('REPLACE_CLIENT', secrets.token_hex(32))
(root / 'config.yaml').write_text(text)
os.chmod(root / 'config.yaml', 0o600)
PY
"${compose[@]}" config --quiet
"${compose[@]}" up -d --wait --wait-timeout 90
# Verify executable architecture, not just successful emulation on the host.
container="$("${compose[@]}" ps -q copilot-proxy)"
docker cp "$container:/usr/local/bin/copilot-proxy" "$tmp/copilot-proxy" >/dev/null
architecture="$(docker image inspect --format '{{.Architecture}}' "$image")"
python3 - "$tmp/copilot-proxy" "$architecture" <<'PYCODE'
import pathlib, struct, sys
header = pathlib.Path(sys.argv[1]).read_bytes()[:64]
assert header[:4] == b'\x7fELF' and header[4] == 2, 'expected 64-bit ELF binary'
assert header[5] in (1, 2), 'invalid ELF byte order'
machine = struct.unpack('<H' if header[5] == 1 else '>H', header[18:20])[0]
expected = {'amd64': 62, 'arm64': 183}[sys.argv[2]]
assert machine == expected, f'image architecture {sys.argv[2]} does not match ELF e_machine={machine}'
print(f'PASS: image architecture {sys.argv[2]} matches its executable')
PYCODE
address="$("${compose[@]}" port copilot-proxy 8317)"
base="http://$address"
[[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/healthz")" == 200 ]]
[[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/v1/models")" == 401 ]]
[[ "$(curl -s -o /dev/null -w '%{http_code}' "$base/v0/management/accounts")" == 401 ]]
curl --fail --silent --show-error "$base/" -o "$tmp/index.html"
python3 - "$tmp/index.html" <<'PY'
import pathlib, sys
html = pathlib.Path(sys.argv[1]).read_text()
assert 'Copilot Proxy' in html
assert '<script type="module" src=' not in html, 'UI depends on external script'
assert '<link rel="stylesheet"' not in html, 'UI depends on external stylesheet'
PY
# Header files keep even these synthetic credentials out of curl's command line.
python3 - "$tmp" <<'PY'
import os, pathlib, re, sys
root = pathlib.Path(sys.argv[1])
text = (root / 'data/config.yaml').read_text()
admin = re.search(r'^management-key: "([^"]+)"$', text, re.M).group(1)
client = re.search(r'^  - "([^"]+)"$', text, re.M).group(1)
for name, key in [('admin', admin), ('client', client)]:
    (root / name).write_text('Authorization: Bearer ' + key + '\n')
    os.chmod(root / name, 0o600)
PY
[[ "$(curl -s -H "@$tmp/client" -o /dev/null -w '%{http_code}' "$base/v1/models")" == 200 ]]
[[ "$(curl -s -H "@$tmp/client" -o /dev/null -w '%{http_code}' "$base/v0/management/accounts")" == 401 ]]
[[ "$(curl -s -H "@$tmp/admin" -o /dev/null -w '%{http_code}' "$base/v1/models")" == 401 ]]
[[ "$(curl -s -H "@$tmp/admin" -o /dev/null -w '%{http_code}' "$base/v0/management/codex-auth-url")" == 404 ]]
# This triggers config.yaml's atomic rename inside the bind-mounted directory.
curl --fail --silent --show-error -H "@$tmp/admin" -X POST "$base/v0/management/api-keys" -o "$tmp/generated-key.json"
[[ "$("${compose[@]}" exec -T copilot-proxy id -u)" == "$COPILOT_UID" ]]
"${compose[@]}" restart copilot-proxy
"${compose[@]}" up -d --wait --wait-timeout 90
address="$("${compose[@]}" port copilot-proxy 8317)"
curl --fail --silent --show-error -H "@$tmp/admin" "http://$address/v0/management/api-keys" -o "$tmp/keys.json"
python3 - "$tmp" <<'PY'
import json, pathlib, sys
root = pathlib.Path(sys.argv[1])
assert len(json.loads((root / 'keys.json').read_text())) == 2, 'API key not persisted across restart'
key = json.loads((root / 'generated-key.json').read_text())['key']
assert key in (root / 'data/config.yaml').read_text(), 'configuration update was not persisted'
assert (root / 'data/config.yaml').stat().st_mode & 0o077 == 0, 'configuration is not private'
PY
printf 'PASS: Compose health, embedded UI, key isolation, writable private config, and restart persistence\n'
