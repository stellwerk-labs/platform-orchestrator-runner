#!/bin/sh
set -eu
source_dir="${1:?source directory required}"
test ! -e "$source_dir"
archive="$(mktemp)"
trap 'rm -f "$archive"' EXIT HUP INT TERM
curl --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 -fsSL \
 https://codeload.github.com/infracost/infracost/tar.gz/aa7176706bc8fed866c8280213ca44cea70954f9 -o "$archive"
printf '%s  %s\n' cb93536076844393e4cf3b4034345879e5462e3b775739544269d78193c67b1d "$archive" | sha256sum -c -
mkdir "$source_dir"
tar -xzf "$archive" --strip-components=1 -C "$source_dir"
