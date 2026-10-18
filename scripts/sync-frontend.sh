#!/usr/bin/env bash
# Sync the built frontend into the plugin embed directory and rewrite the
# "resources" block in plugin/main.go so registered paths always match the
# embedded hashed asset names. Idempotent.
#
# Usage: bash scripts/sync-frontend.sh [--no-build]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ "${1:-}" != "--no-build" ]]; then
	(cd frontend && npm run build)
fi

if [[ ! -f frontend/dist/index.html ]]; then
	echo "error: frontend/dist/index.html missing — build the frontend first" >&2
	exit 1
fi

mkdir -p plugin/web/assets
rm -f plugin/web/assets/*
cp frontend/dist/index.html plugin/web/index.html
cp frontend/dist/assets/* plugin/web/assets/

# Regenerate the resources entries in plugin/main.go from the files that now
# exist under plugin/web/assets/. The first /report entry is preserved.
node - <<'EOF'
const fs = require('fs');
const files = fs.readdirSync('plugin/web/assets').sort();
if (files.length === 0) {
	console.error('error: plugin/web/assets is empty');
	process.exit(1);
}
const entries = files.map(f => {
	let desc = 'dashboard asset';
	if (f.endsWith('.js')) desc = 'dashboard js';
	else if (f.endsWith('.css')) desc = 'dashboard css';
	return '\t\t\t{"Path":"/assets/' + f + '","Menu":"","Description":"' + desc + '"}';
});
const block = '"resources":[\n' +
	'\t\t\t{"Path":"/report","Menu":"CPA Token Analytics","Description":"CPA usage dashboard"},\n' +
	entries.join(',\n') +
	'\n\t\t]';
const main = 'plugin/main.go';
const src = fs.readFileSync(main, 'utf8');
const re = /"resources":\[[\s\S]*?\n\t\t\]/;
if (!re.test(src)) {
	console.error('error: resources block not found in plugin/main.go');
	process.exit(1);
}
const out = src.replace(re, () => block);
if (out !== src) fs.writeFileSync(main, out);
console.log('synced assets: ' + files.join(', '));
EOF
