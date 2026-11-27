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
test "$(sha256sum "$bundle/opentofu/SOURCE.json" | cut -d ' ' -f 1)" = d9597c7f56434b3c4d8e364644b779e06eda53f8f1b4e226dc9fea3cadad513f
test "$(sha256sum "$bundle/infracost/LICENSE" | cut -d ' ' -f 1)" = de34de041a08b4396b411d3c173132bbcafe902f90307cca5cc00aae00ae6bad
test "$(sha256sum "${INFRACOST_BINARY:-/opt/runner/infracost}" | cut -d ' ' -f 1)" = "$(cat "$bundle/infracost/binary.sha256")"
test "$(sha256sum "$bundle/infracost/SOURCE.json" | cut -d ' ' -f 1)" = 37c1d38e252d119b7b699a0dc5c84fb2c201d4e4c22666eaa94379e0a8c53edb
test "$(sha256sum "$bundle/infracost/build/PATCHES.json" | cut -d ' ' -f 1)" = f0c32a6d28351445a97781315900291060f18961733808c17f12bd826e5f2c7a
test "$(sha256sum "$bundle/infracost/corresponding-source.tar.gz" | cut -d ' ' -f 1)" = e7d5f42c25cccd860110a1eb80fcc094d5e0b0058c2432e558814695a57b38b4
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
