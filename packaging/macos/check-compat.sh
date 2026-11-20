#!/usr/bin/env bash
# Fails if a Mach-O binary wouldn't load on the oldest macOS we support
# (LSMinimumSystemVersion): a deployment target above it, or a hard link to a
# framework that doesn't exist there (ScreenCaptureKit is macOS 12.3+).
set -euo pipefail
min="${MACOS_MIN:-11.0}"
status=0
for bin in "$@"; do
  for arch in $(lipo -archs "$bin"); do
    v=$(otool -arch "$arch" -l "$bin" | awk '
      /LC_BUILD_VERSION/ {b=1} b && $1=="minos" {print $2; exit}
      /LC_VERSION_MIN_MACOSX/ {m=1} m && $1=="version" {print $2; exit}')
    if [ -z "$v" ] || [ "$(printf '%s\n%s\n' "$v" "$min" | sort -V | tail -1)" != "$min" ]; then
      echo "::error::$bin ($arch): deployment target '${v:-?}' is above macOS $min"; status=1
    fi
    if otool -arch "$arch" -L "$bin" | grep -q ScreenCaptureKit; then
      echo "::error::$bin ($arch): links ScreenCaptureKit (won't load before macOS 12.3)"; status=1
    fi
    echo "$bin ($arch): minos $v"
  done
done
exit $status
