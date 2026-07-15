#!/usr/bin/env bash
# Cuts the next release: computes the next version from existing v* tags,
# tags HEAD, and pushes the tag. CI publishes the versioned image from there.
set -euo pipefail

bump="${1:-minor}"
case "$bump" in
  major|minor|patch) ;;
  *) echo "usage: scripts/release.sh [major|minor|patch]" >&2; exit 1 ;;
esac

if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: working tree is not clean" >&2
  exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$branch" != "main" ]]; then
  echo "error: releases are cut from main (currently on $branch)" >&2
  exit 1
fi

git fetch --tags --quiet origin
git pull --ff-only --quiet origin main

latest="$(git tag --list 'v*' --sort=-v:refname | head -n1)"
major=0 minor=0 patch=0
if [[ -n "$latest" ]]; then
  IFS=. read -r major minor patch <<<"${latest#v}"
fi

case "$bump" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
next="v${major}.${minor}.${patch}"

echo "Latest release: ${latest:-none}"
echo "Next release:   ${next}"

git tag -a "$next" -m "Release $next"
git push origin "$next"
echo "Tagged and pushed $next — CI will publish ghcr.io/jclement/slayground:${next#v}"
