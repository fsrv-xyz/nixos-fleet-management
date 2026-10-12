#!/usr/bin/env bash
# Sets vendorHash in nix/package.nix to the hash of the current Go dependencies.
set -euo pipefail

package=nix/package.nix
original=$(cat "$package")
restore() { printf '%s\n' "$original" >"$package"; }

sed -E 's|vendorHash = "[^"]*";|vendorHash = lib.fakeHash;|' <<<"$original" >"$package"
if output=$(nix build --no-link .#default.goModules 2>&1); then
  restore
  echo "vendorHash check passed with lib.fakeHash, expected a hash mismatch" >&2
  exit 1
fi
hash=$(sed -nE 's/.*got: +(sha256-[A-Za-z0-9+/=]+).*/\1/p' <<<"$output" | head -n 1)
if [ -z "$hash" ]; then
  restore
  printf '%s\n' "$output" >&2
  exit 1
fi
sed -E 's|vendorHash = "[^"]*";|vendorHash = "'"$hash"'";|' <<<"$original" >"$package"
echo "vendorHash = $hash"
