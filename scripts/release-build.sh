#!/usr/bin/env bash
# The release's files (docs/release.md): for macOS and Linux, amd64 and
# arm64, kairocode_<version>_<os>_<arch>.tar.gz (kairocode, kairo linked to
# it, the licences, the README), checksums.txt, and kairocode.rb, the
# Homebrew formula for the tap.
#   scripts/release-build.sh v0.1.0 dist
# Pure Go (CGO_ENABLED=0): every one is built here, on any of them.
set -euo pipefail

version=${1:?usage: scripts/release-build.sh <version: v0.1.0> <out dir>}
out=${2:?usage: scripts/release-build.sh <version: v0.1.0> <out dir>}
case $version in
  v[0-9]*) ;;
  *) echo "release-build: the version is a tag, v0.1.0: $version" >&2; exit 1 ;;
esac
repo=r-hashi01/kairocode

rm -rf "$out"
mkdir -p "$out"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for os in darwin linux; do
  for arch in amd64 arm64; do
    name="kairocode_${version#v}_${os}_${arch}"
    dir="$work/$name"
    mkdir -p "$dir/third_party"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
      -ldflags "-s -w -X github.com/$repo/internal/cli.Version=$version" \
      -o "$dir/kairocode" ./cmd/kairocode
    ln -s kairocode "$dir/kairo"
    cp LICENSE README.md "$dir/"
    cp -R third_party/codex-sandbox "$dir/third_party/"
    tar -C "$work" -czf "$out/$name.tar.gz" "$name"
  done
done

(cd "$out" && { command -v sha256sum >/dev/null && sha256sum ./*.tar.gz || shasum -a 256 ./*.tar.gz; } | sed 's| \./| |' > checksums.txt)

sum() { awk -v f="kairocode_${version#v}_$1.tar.gz" '$2 == f {print $1}' "$out/checksums.txt"; }
url() { echo "https://github.com/$repo/releases/download/$version/kairocode_${version#v}_$1.tar.gz"; }

cat > "$out/kairocode.rb" <<RUBY
# Made by scripts/release-build.sh for $version: put it in the tap
# (Formula/kairocode.rb of r-hashi01/homebrew-tap).
class Kairocode < Formula
  desc "Coding agent whose sessions survive crashes, long waits and running side by side"
  homepage "https://github.com/$repo"
  version "${version#v}"
  license "MIT"

  on_macos do
    on_arm do
      url "$(url darwin_arm64)"
      sha256 "$(sum darwin_arm64)"
    end
    on_intel do
      url "$(url darwin_amd64)"
      sha256 "$(sum darwin_amd64)"
    end
  end

  on_linux do
    depends_on "bubblewrap" # its sandbox (ADR 0005)
    on_arm do
      url "$(url linux_arm64)"
      sha256 "$(sum linux_arm64)"
    end
    on_intel do
      url "$(url linux_amd64)"
      sha256 "$(sum linux_amd64)"
    end
  end

  def install
    bin.install "kairocode"
    bin.install_symlink "kairocode" => "kairo"
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/kairocode version")
  end
end
RUBY

ls -l "$out"
