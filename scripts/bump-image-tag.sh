#!/usr/bin/env bash
# Writes an image tag into deploy/helm/maiak/values-platform.yaml and pushes it to the main branch.
# Used by .github/workflows/release.yml (jobs bump-backend and bump-web).
#
#   bump-image-tag.sh backend <sha>   rewrites `image.tag`
#   bump-image-tag.sh web <sha>       rewrites `web.image.tag`
#
# Race handling: when the push is rejected because another bump landed first, the script fetches
# the new tip, re-applies its one-line edit on top of it and pushes again (the equivalent of a
# rebase of a one-line change, without conflicts). It only ever commits the values file and never
# resets or discards anything else in the working tree.
#
# Environment (defaults in brackets):
#   BUMP_FILE      [deploy/helm/maiak/values-platform.yaml]
#   BUMP_REMOTE    [origin]
#   BUMP_BRANCH    [main]
#   BUMP_ATTEMPTS  [5]
#   BUMP_NO_PUSH   set to 1 to only rewrite BUMP_FILE in place (used to test the edit on a copy)
set -euo pipefail

usage() {
  echo "usage: $0 backend|web <short-sha>" >&2
  exit 2
}

[ $# -eq 2 ] || usage
which=$1
sha=$2
case "$which" in backend | web) ;; *) usage ;; esac
if ! [[ "$sha" =~ ^[0-9a-f]{7,40}$ ]]; then
  echo "invalid sha: $sha" >&2
  exit 2
fi

file=${BUMP_FILE:-deploy/helm/maiak/values-platform.yaml}
remote=${BUMP_REMOTE:-origin}
branch=${BUMP_BRANCH:-main}
attempts=${BUMP_ATTEMPTS:-5}

# Replaces the quoted value of exactly one key: `tag:` under top-level `image:` (backend) or
# `tag:` under `image:` under top-level `web:` (web). Fails unless exactly one line matched.
rewrite() {
  local tmp
  tmp=$(mktemp)
  if ! awk -v which="$which" -v sha="$sha" '
    /^[^ #]/ { top = $0; sub(/:.*/, "", top); second = "" }
    /^  [^ #]/ { second = $0; sub(/^  /, "", second); sub(/:.*/, "", second) }
    {
      if (which == "backend" && top == "image" && $0 ~ /^  tag: "[^"]*"/) { sub(/"[^"]*"/, "\"" sha "\""); n++ }
      else if (which == "web" && top == "web" && second == "image" && $0 ~ /^    tag: "[^"]*"/) { sub(/"[^"]*"/, "\"" sha "\""); n++ }
      print
    }
    END { if (n != 1) exit 3 }
  ' "$file" >"$tmp"; then
    rm -f "$tmp"
    echo "could not rewrite the $which tag in $file (expected exactly one match)" >&2
    return 1
  fi
  cat "$tmp" >"$file"
  rm -f "$tmp"
}

if [ "${BUMP_NO_PUSH:-}" = "1" ]; then
  rewrite
  exit 0
fi

for attempt in $(seq 1 "$attempts"); do
  git fetch --quiet --no-tags "$remote" "$branch"
  # Start from the current tip. The only local commit that can exist is our own earlier bump,
  # which was rejected and is recreated below.
  git checkout --quiet --detach "$remote/$branch"
  rewrite
  if git diff --quiet -- "$file"; then
    echo "$which tag already $sha on $remote/$branch; nothing to commit."
    exit 0
  fi
  git add -- "$file"
  git commit --quiet -m "chore(deploy): maiak-$which $sha [skip ci]"
  if git push --quiet "$remote" "HEAD:$branch"; then
    echo "pushed $which tag $sha (attempt $attempt)"
    exit 0
  fi
  echo "push raced (attempt $attempt of $attempts); retrying on the new tip"
  sleep $((attempt * 2 + RANDOM % 3))
done
echo "failed to push the $which tag bump after $attempts attempts" >&2
exit 1
