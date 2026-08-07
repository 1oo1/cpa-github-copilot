#!/bin/sh

set -eu

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
source_url=${COMPATIBILITY_SOURCE_URL:-https://pi.dev/api/models/providers/github-copilot}
manifest_path="$repo_root/src/compatibility.json"
models_tmp=$(mktemp "${TMPDIR:-/tmp}/cpa-github-copilot-models.XXXXXX")
manifest_tmp=$(mktemp "$manifest_path.tmp.XXXXXX")

cleanup() {
	rm -f "$models_tmp" "$manifest_tmp"
}

trap cleanup EXIT HUP INT TERM

curl \
	--fail \
	--silent \
	--show-error \
	--location \
	--retry 3 \
	--connect-timeout 10 \
	--max-time 60 \
	--output "$models_tmp" \
	"$source_url"

jq -e '
	type == "object" and
	length > 0 and
	all(to_entries[];
		(.key | type == "string") and
		(.value | type == "object") and
		(.value.id == .key)
	)
' "$models_tmp" >/dev/null

jq --indent 4 --slurpfile models "$models_tmp" \
	'.models = $models[0]' "$manifest_path" >"$manifest_tmp"

mv "$manifest_tmp" "$manifest_path"