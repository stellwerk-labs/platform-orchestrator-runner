#!/bin/sh
set -eu
source_dir="${1:?source directory required}"
test ! -e "$source_dir"
archive="$(mktemp)"
trap 'rm -f "$archive"' EXIT HUP INT TERM
curl --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 -fsSL \
 https://codeload.github.com/infracost/infracost/tar.gz/40ad0ac9f247d883884cd0f60057367b4af3df08 -o "$archive"
printf '%s  %s\n' c186b132419769525508623b29a1122a5af006f63136e96abdedf5f12ec2cce3 "$archive" | sha256sum -c -
mkdir "$source_dir"
tar -xzf "$archive" --strip-components=1 -C "$source_dir"
