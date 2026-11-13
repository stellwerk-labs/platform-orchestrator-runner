#!/bin/sh
# Collect only production packages for the selected target architecture.
set -eu
root="$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/release-notice-tools.sh
. "$root/scripts/release-notice-tools.sh"
output="${1:?usage: collect-licenses.sh OUTPUT PACKAGE ...}"
shift
test "$#" -gt 0
test ! -e "$output"
case "${GOARCH:-}" in amd64|arm64) ;; *) echo "explicit supported GOARCH required" >&2; exit 1 ;; esac
export GOTOOLCHAIN="$release_go_version" GOWORK=off GOENV=off CGO_ENABLED=0 GOOS=linux GOAMD64=v1 GOARM64=v8.0
unset GOFLAGS GOEXPERIMENT
go version -m "$(command -v go-licenses)" | grep -F "$go_licenses_module" | grep -F "$go_licenses_version" >/dev/null
module="$(go list -m -f '{{.Path}}')"
work="$(mktemp -d)"
trap 'rm -r -- "$work"' EXIT HUP INT TERM
# Go module collection ignores the application module. External dependencies require notices.
go list -deps -f '{{with .Module}}{{.Path}}|{{.Version}}|{{.Dir}}|{{with .Replace}}{{if .Version}}remote{{else}}local{{end}}{{end}}{{end}}' "$@" > "$work/modules"
sort -u "$work/modules" | sed '/^$/d' > "$work/unique"
ignore="$module"
mkdir "$work/declared"
while IFS='|' read -r dependency version module_dir replacement; do
  case "$dependency" in
    "$module") continue ;;
    "$module"*) echo "external module collides with application ignore prefix" >&2; exit 1 ;;
  esac
  declaration="$(awk -F '|' -v module="$dependency" '$1 == module {print}' "$root/scripts/declared-licenses.tsv")"
  if test -z "$declaration"; then
    continue
  fi
  checksum="$(printf '%s\n' "$declaration" | cut -d '|' -f 3)"
  license_blob="$(printf '%s\n' "$declaration" | cut -d '|' -f 4)"
  test -f "$module_dir/LICENSE" && test ! -L "$module_dir/LICENSE"
  test "$(sha256sum "$module_dir/LICENSE" | awk '{print $1}')" = "$checksum"
  receipt="$work/declared/$(printf '%s' "$dependency" | tr '/' '_')"
  test -n "$version" && test -z "$replacement" || { echo "declared dependency must be an unreplaced release" >&2; exit 1; }
  metadata="$(go mod download -json "$dependency@$version")"
  origin="$(printf '%s\n' "$metadata" | sed -n 's/^[[:space:]]*"Hash": "\([0-9a-f]\{40\}\)"[,]*$/\1/p')"
  archive_sum="$(printf '%s\n' "$metadata" | sed -n 's/^[[:space:]]*"Sum": "\(h1:[^"]*\)"[,]*$/\1/p')"
  if ! printf '%s\n' "$origin" | grep -Eq '^[0-9a-f]{40}$'; then
    echo "declared dependency has no exact source commit" >&2
    exit 1
  fi
  if ! printf '%s\n' "$archive_sum" | grep -Eq '^h1:[A-Za-z0-9+/]{43}=$'; then
    echo "declared dependency has no valid module checksum" >&2
    exit 1
  fi
  printf '{"module": "%s", "version": "%s", "sourceCommit": "%s", "moduleSum": "%s", "licenseBlob": "%s", "licenseSHA256": "%s", "spdx": "EUPL-1.2"}\n' \
    "$dependency" "$version" "$origin" "$archive_sum" "$license_blob" "$checksum" > "$receipt"
  ignore="$ignore,$dependency"
done < "$work/unique"
# go-licenses uses package prefixes, not module identities. Ensure a declared
# declaration cannot silently cover a different module with a similar name.
while IFS='|' read -r dependency version module_dir replacement; do
  if awk -F '|' -v module="$dependency" '$1 == module {found=1} END {exit !found}' "$root/scripts/declared-licenses.tsv"; then continue; fi
  while IFS='|' read -r sdk _spdx checksum license_blob; do
    case "$sdk" in ""|\#*) continue ;; esac
    case "$dependency" in "$sdk") ;; "$sdk"*) echo "external module collides with declared dependency prefix" >&2; exit 1 ;; esac
  done < "$root/scripts/declared-licenses.tsv"
done < "$work/unique"
# Prefixes are limited to this repository and byte-verified, explicitly declared
# modules. Dependencies of ignored packages remain checked by go-licenses.
go-licenses save "$@" --ignore "$ignore" --save_path "$output"
mkdir -p "$output/application" "$output/go"
cp "$root/LICENSE" "$output/application/LICENSE"
goroot="$(go env GOROOT)"
cp "$goroot/LICENSE" "$goroot/PATENTS" "$output/go/"
cut -d '|' -f 1,2 "$work/unique" > "$output/modules.txt"
printf 'linux/%s\n' "$GOARCH" > "$output/target.txt"
# go-licenses saves recognized license texts, but can omit PATENTS/NOTICE.
# Include legal notices from each contributing package through its module root.
go list -deps -f '{{if .Module}}{{.Module.Path}}|{{.Module.Dir}}|{{.Dir}}{{end}}' "$@" > "$work/packages"
while IFS='|' read -r dependency module_dir package_dir; do
  case "$dependency" in ""|"$module") continue ;; esac
  test -d "$module_dir"
  case "$package_dir/" in "$module_dir/"*) ;; *) echo "package outside dependency module" >&2; exit 1 ;; esac
  current="$package_dir"
  while :; do
    for notice in "$current"/LICENSE* "$current"/LICENCE* "$current"/COPYING* "$current"/NOTICE* "$current"/PATENTS*; do
      test -e "$notice" || continue
      test ! -L "$notice" || { echo "symlinked legal notice" >&2; exit 1; }
      test -f "$notice" || continue
      relative="${notice#"$module_dir"/}"
      destination="$output/$dependency/$relative"
      mkdir -p "$(dirname "$destination")"
      if test -e "$destination"; then
        cmp "$notice" "$destination" >/dev/null
      else
        cp "$notice" "$destination"
      fi
    done
    test "$current" != "$module_dir" || break
    current="$(dirname "$current")"
  done
done < "$work/packages"
while IFS='|' read -r dependency version module_dir replacement; do
  receipt="$work/declared/$(printf '%s' "$dependency" | tr '/' '_')"
  if test -f "$receipt"; then
    mkdir -p "$output/$dependency"
    cp "$receipt" "$output/$dependency/LICENSE.provenance"
  fi
done < "$work/unique"
# Preserve the complete authenticated upstream notice set, not just its binary.
: "${OPENTOFU_NOTICES_DIR:?authenticated OpenTofu notice directory required}"
mkdir "$output/opentofu"
for notice in LICENSE README.md CHANGELOG.md; do
  test -f "$OPENTOFU_NOTICES_DIR/$notice" && test ! -L "$OPENTOFU_NOTICES_DIR/$notice"
  cp "$OPENTOFU_NOTICES_DIR/$notice" "$output/opentofu/$notice"
done
cp "$root/scripts/release-notices/opentofu-source.json" "$output/opentofu/SOURCE.json"
sh "$root/scripts/collect-infracost-licenses.sh" "$output/infracost"
target_arch="$GOARCH"
# The verifier runs on the build host, while querying the actual target graph.
(unset GOOS GOARCH GOAMD64 GOARM64; go run ./scripts/noticecheck "$output" "$root" "$target_arch")
(cd "$output"; find . -type f | LC_ALL=C sort > "$work/files"
 while IFS= read -r notice; do sha256sum "$notice"; done < "$work/files") > "$work/manifest"
mv "$work/manifest" "$output/manifest.sha256"
sh "$root/scripts/validate-notices.sh" "$output"
