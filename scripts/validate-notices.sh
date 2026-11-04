#!/bin/sh
# Validate the actual final copy using only the existing Alpine runtime tools.
set -eu
bundle="${1:?notice directory required}"
test -d "$bundle" && test ! -L "$bundle"
for required in application/LICENSE go/LICENSE go/PATENTS modules.txt target.txt manifest.sha256 opentofu/LICENSE opentofu/README.md opentofu/CHANGELOG.md opentofu/SOURCE.json; do
  test -f "$bundle/$required" && test -s "$bundle/$required" && test ! -L "$bundle/$required"
done
test -z "$(find "$bundle" -type l -print)"
test "$(sha256sum "$bundle/opentofu/LICENSE" | cut -d ' ' -f 1)" = 142b957651baf401646dd7f3d855cb7c4f6f0921d645faa27e7cb540bc33cf4c
test "$(sha256sum "$bundle/opentofu/SOURCE.json" | cut -d ' ' -f 1)" = 224f71756dd0832e678cdbb950bdcfe7d2966c4e32d2a76e77bccd4c0180ba6c
work="$(mktemp -d)"
trap 'rm -r -- "$work"' EXIT HUP INT TERM
cd "$bundle"
# Reject unsafe names, duplicate rows, and extra or missing files, not just bad hashes.
if grep -Ev '^[0-9a-f]{64}  \./[A-Za-z0-9_./+@()-]+$' manifest.sha256 > "$work/invalid"; then exit 1; fi
if grep -E '/\.\.?/' manifest.sha256 > "$work/invalid"; then exit 1; fi
sed 's/^[0-9a-f]*  //' manifest.sha256 | LC_ALL=C sort > "$work/listed"
LC_ALL=C sort -u "$work/listed" > "$work/unique"
cmp "$work/listed" "$work/unique"
find . -type f ! -path './manifest.sha256' | LC_ALL=C sort > "$work/actual"
cmp "$work/listed" "$work/actual"
sha256sum -c manifest.sha256 >/dev/null
