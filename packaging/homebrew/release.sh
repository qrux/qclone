#!/usr/bin/env bash
# release.sh VERSION -- publish qclone VERSION through the Homebrew tap
# <owner>/homebrew-tap, so `brew install <owner>/tap/qclone` gets it.
#
#   make release VERSION=0.2.0            tag, push the tag, write and push the formula
#   make release VERSION=0.2.0 DRY_RUN=1  the same, but nothing is pushed
#
# The owner, the repo and the ssh host alias all come from origin's URL
# (git@github-qrux:qrux/qclone.git), so the tap is git@<alias>:<owner>/homebrew-tap.git.
#
# 1. A new tag: main must be clean and equal to GitHub's, and vet and the tests
#    must pass; then vVERSION is made at HEAD and pushed. A tag already on
#    GitHub is used as it is (this republishes its formula).
# 2. GitHub's tarball for the tag is downloaded and its SHA-256 taken, and it
#    is built the way the formula builds it: `qclone --version` must print
#    "qclone VERSION".
# 3. The tap is cloned (or refreshed) into build/homebrew-tap; Formula/qclone.rb
#    is written from qclone.rb.in, committed with this repo's git identity, and
#    pushed.
#
# Prints no local paths: it runs under the project's log tool.
set -euo pipefail

say() { printf '==> %s\n' "$*"; }
die() { printf 'release: %s\n' "$*" >&2; exit 1; }
# A push is the one step DRY_RUN leaves out.
push() { if [ -n "${DRY_RUN:-}" ]; then say "DRY_RUN: would run: git $*"; else git "$@"; fi; }

VERSION="${1:-}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "usage: make release VERSION=x.y.z [DRY_RUN=1]"
cd "$(git rev-parse --show-toplevel)"

origin="$(git remote get-url origin)"
[[ "$origin" =~ ^git@([^:]+):([^/]+)/([^/]+)$ ]] || die "origin is not an ssh GitHub URL (git@host:owner/repo.git)"
host="${BASH_REMATCH[1]}" owner="${BASH_REMATCH[2]}" repo="${BASH_REMATCH[3]%.git}"
tag="v$VERSION"
tap_url="${TAP_URL:-git@$host:$owner/homebrew-tap.git}"   # TAP_URL: for testing against a local repo
name="$(git config user.name)" email="$(git config user.email)"
[ -n "$name" ] && [ -n "$email" ] || die "this repo has no git identity of its own"

# --- 1. the tag
git fetch -q --tags origin
remote_tag="$(git ls-remote origin "refs/tags/$tag^{}" | cut -f1)"
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
  [ -n "$remote_tag" ] || die "$tag exists here but not on GitHub: push it, or delete it here"
  [ "$(git rev-parse "$tag^{commit}")" = "$remote_tag" ] || die "$tag is a different commit here and on GitHub"
  say "$tag is already on GitHub ($(git rev-parse --short "$tag^{commit}")); publishing its formula"
else
  [ -z "$remote_tag" ] || die "$tag is on GitHub but not here: git fetch --tags"
  [ "$(git rev-parse --abbrev-ref HEAD)" = main ] || die "not on main"
  [ -z "$(git status --porcelain)" ] || die "uncommitted changes"
  [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "main is not what GitHub has: push or pull first"
  say "vet and tests"
  go vet ./...
  go test -count=1 ./...
  if [ -n "${DRY_RUN:-}" ]; then
    say "DRY_RUN: would tag $tag at $(git rev-parse --short HEAD) and push it; the rest needs the tag on GitHub, so stopping here"
    exit 0
  fi
  git tag -a "$tag" -m "qclone $VERSION"
  push push origin "$tag"
fi

# --- 2. the tarball
work=build/release
rm -rf "$work"   # this script's own scratch from an earlier run
mkdir -p "$work/src"
url="https://github.com/$owner/$repo/archive/refs/tags/$tag.tar.gz"
for try in 1 2 3 4 5; do
  curl -sSfL -m 120 -o "$work/src.tar.gz" "$url" && break
  [ "$try" = 5 ] && die "could not download $url"
  sleep 3
done
sha="$(shasum -a 256 "$work/src.tar.gz" | cut -d' ' -f1)"
tar -xzf "$work/src.tar.gz" -C "$work/src" --strip-components 1
(cd "$work/src" && go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o ../qclone ./cmd/qclone)
got="$("$work/qclone" --version)"
[ "$got" = "qclone $VERSION" ] || die "the tarball builds a qclone that says '$got'"
say "tarball $url"
say "sha256 $sha; builds; says '$got'"

# --- 3. the tap
tap=build/homebrew-tap
if [ -d "$tap/.git" ]; then
  git -C "$tap" fetch -q origin
  if git -C "$tap" rev-parse -q --verify origin/main >/dev/null; then
    git -C "$tap" checkout -q -B main origin/main
  fi
else
  git clone -q "$tap_url" "$tap" 2>/dev/null || die "can't reach $owner/homebrew-tap on GitHub: create it there (public, empty) first"
  git -C "$tap" symbolic-ref HEAD refs/heads/main
fi
git -C "$tap" config user.name "$name"
git -C "$tap" config user.email "$email"
mkdir -p "$tap/Formula"
fill() { sed -e "s/@OWNER@/$owner/g" -e "s/@REPO@/$repo/g" -e "s/@VERSION@/$VERSION/g" -e "s/@SHA256@/$sha/g" "$1"; }
fill packaging/homebrew/qclone.rb.in > "$tap/Formula/qclone.rb"
[ -f "$tap/README.md" ] || fill packaging/homebrew/tap-README.md > "$tap/README.md"
git -C "$tap" add -A
if git -C "$tap" diff --cached --quiet; then
  say "the tap already has this formula"
else
  git -C "$tap" commit -q -m "qclone $VERSION"
  say "tap commit $(git -C "$tap" rev-parse --short HEAD) by $name"
fi
push -C "$tap" push -q -u origin main
say "done: brew install $owner/tap/qclone"
