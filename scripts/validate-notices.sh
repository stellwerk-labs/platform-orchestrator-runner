#!/bin/sh
# Validate the actual final copy using only the existing Alpine runtime tools.
set -eu
bundle="${1:?notice directory required}"
test -d "$bundle" && test ! -L "$bundle"
for required in application/LICENSE go/LICENSE go/PATENTS modules.txt target.txt manifest.sha256 opentofu/LICENSE opentofu/README.md opentofu/CHANGELOG.md opentofu/SOURCE.json opentofu/SOURCE-AVAILABILITY.txt opentofu/corresponding-source.tar.gz opentofu/build/BUILD.json opentofu/build/LICENSE opentofu/build/toolchain-input.json opentofu/build/go.mod opentofu/build/go.sum opentofu/build/build-opentofu.sh opentofu/go/LICENSE opentofu/go/PATENTS opentofu/dependencies/github.com/jmespath/go-jmespath/LICENSE opentofu/dependencies/github.com/jmespath/go-jmespath/APACHE-2.0.txt opentofu/modules.txt opentofu/buildinfo.txt opentofu/binary.sha256 opentofu/target.txt opentofu/licenses.csv infracost/LICENSE infracost/SOURCE-AVAILABILITY.txt infracost/corresponding-source.tar.gz infracost/SOURCE.json infracost/build/PATCHES.json infracost/build/go.mod infracost/build/go.sum infracost/modules.txt infracost/buildinfo.txt infracost/binary.sha256 infracost/infracost.spdx.json; do
  test -f "$bundle/$required" && test -s "$bundle/$required" && test ! -L "$bundle/$required"
done
test -z "$(find "$bundle" -type l -print)"
test "$(sha256sum "$bundle/opentofu/LICENSE" | cut -d ' ' -f 1)" = 142b957651baf401646dd7f3d855cb7c4f6f0921d645faa27e7cb540bc33cf4c
test "$(sha256sum "$bundle/opentofu/dependencies/github.com/jmespath/go-jmespath/LICENSE" | cut -d ' ' -f 1)" = 03cfaf331a260694d06227a589cdd2b5fcfab474697b72e250736696c313655a
test "$(sha256sum "$bundle/opentofu/dependencies/github.com/jmespath/go-jmespath/APACHE-2.0.txt" | cut -d ' ' -f 1)" = 1ae07514be1d7bb33f0698f8d91fb51b8b9fe1463157ec1c72081a49b9bc6f40
test "$(sha256sum "$bundle/opentofu/SOURCE.json" | cut -d ' ' -f 1)" = eb956b5d0884862c0e5446df7c64b06176c1dd9661d69980cf44433d70a6bf62
test "$(sha256sum "$bundle/opentofu/build/BUILD.json" | cut -d ' ' -f 1)" = 1245f2205f5d4ad0ba2f6a761fe068e1b63168296b5eb8075eac511e646c20e9
test "$(sha256sum "${OPENTOFU_BINARY:-/usr/local/bin/tofu}" | cut -d ' ' -f 1)" = "$(cat "$bundle/opentofu/binary.sha256")"
target="$(cat "$bundle/target.txt")"
case "$target" in linux/amd64|linux/arm64) ;; *) exit 1 ;; esac
expected_source="$(awk -v key="\"$target\":" '$1 == key {gsub(/[\",]/,"",$2); print $2}' "$bundle/opentofu/SOURCE.json")"
printf '%s\n' "$expected_source" | grep -Eq '^[0-9a-f]{64}$'
test "$(sha256sum "$bundle/opentofu/corresponding-source.tar.gz" | cut -d ' ' -f 1)" = "$expected_source"
test "$(sha256sum "$bundle/infracost/LICENSE" | cut -d ' ' -f 1)" = de34de041a08b4396b411d3c173132bbcafe902f90307cca5cc00aae00ae6bad
test "$(sha256sum "${INFRACOST_BINARY:-/opt/runner/infracost}" | cut -d ' ' -f 1)" = "$(cat "$bundle/infracost/binary.sha256")"
test "$(sha256sum "$bundle/infracost/SOURCE.json" | cut -d ' ' -f 1)" = 65cdfab6c69834f02123ec68a72c86bb260ab2bd6e029e034d122d98d34c0d1f
test "$(sha256sum "$bundle/infracost/build/PATCHES.json" | cut -d ' ' -f 1)" = 369159fab74b4b42282540507953838520dfab4e33d5f94bbfc99e5a9b3a971d
test "$(sha256sum "$bundle/infracost/corresponding-source.tar.gz" | cut -d ' ' -f 1)" = 389fe24458907c795ca48a6e2bb6745db3013ff4ab33d03ece79f7cca87f3c73
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
