#!/bin/sh
# Validate the actual final copy using only the existing Alpine runtime tools.
set -eu
bundle="${1:?notice directory required}"
test -d "$bundle" && test ! -L "$bundle"
for required in application/LICENSE go/LICENSE go/PATENTS modules.txt target.txt manifest.sha256 opentofu/LICENSE opentofu/README.md opentofu/CHANGELOG.md opentofu/SOURCE.json infracost/LICENSE infracost/SOURCE-AVAILABILITY.txt infracost/corresponding-source.tar.gz infracost/SOURCE.json infracost/build/PATCHES.json infracost/build/go.mod infracost/build/go.sum infracost/modules.txt infracost/buildinfo.txt infracost/binary.sha256 infracost/infracost.spdx.json; do
  test -f "$bundle/$required" && test -s "$bundle/$required" && test ! -L "$bundle/$required"
done
test -z "$(find "$bundle" -type l -print)"
test "$(sha256sum "$bundle/opentofu/LICENSE" | cut -d ' ' -f 1)" = 142b957651baf401646dd7f3d855cb7c4f6f0921d645faa27e7cb540bc33cf4c
test "$(sha256sum "$bundle/opentofu/SOURCE.json" | cut -d ' ' -f 1)" = 224f71756dd0832e678cdbb950bdcfe7d2966c4e32d2a76e77bccd4c0180ba6c
test "$(sha256sum "$bundle/infracost/LICENSE" | cut -d ' ' -f 1)" = de34de041a08b4396b411d3c173132bbcafe902f90307cca5cc00aae00ae6bad
test "$(sha256sum "${INFRACOST_BINARY:-/opt/runner/infracost}" | cut -d ' ' -f 1)" = "$(cat "$bundle/infracost/binary.sha256")"
test "$(sha256sum "$bundle/infracost/SOURCE.json" | cut -d ' ' -f 1)" = e03529cfa4a1803cef8748fff0a15d65785dfe12917b3f0096042174afe5f81f
test "$(sha256sum "$bundle/infracost/build/PATCHES.json" | cut -d ' ' -f 1)" = c75b4b058e2820aa2e90c40285280b7c0f6a48c994546b68fba7c00b53ff08ed
test "$(sha256sum "$bundle/infracost/corresponding-source.tar.gz" | cut -d ' ' -f 1)" = 95aaa49dd3e403c073d72357cd6a019490a534f234c70686cf91878eea2ad8e8
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
