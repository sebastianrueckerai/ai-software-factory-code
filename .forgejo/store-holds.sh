#!/usr/bin/env bash
# .forgejo/store-holds.sh <Dockerfile>... passes when the store holds every
# input the Dockerfiles read: each line of upstream-images.txt whose digest
# a Dockerfile names, each line of upstream-files.txt whose sha256 it
# names, and the go-modules and npm-cache tarballs of go.sum and the plugin
# lockfile when it fetches them. factoryd copies what a pushed head adds
# before its checks, but an open pull request's checks start at the push,
# beside that copy: the script waits for it, at most STORE_WAIT seconds,
# and then names each input still missing:
#   the store lacks <package> <version> <file>: the copy before the checks did not make it
# The store is org upstream of the Forgejo at STORE, read without a login.
set -euo pipefail
store=${STORE:?the Forgejo whose org upstream is the store}
store=${store%/}
wait=${STORE_WAIT:-1800}
sha() { sha256sum "$1" | cut -d' ' -f1; }
needs=()
for df in "$@"; do
  while read -r ref _; do
    case "$ref" in '' | '#'* | https://*) continue ;; esac
    if grep -qF "@${ref#*@}" "$df"; then
      image=${ref%@*}
      needs+=("image ${image%:*} ${image##*:} ${ref#*@}")
    fi
  done < upstream-images.txt
  while read -r package version name _ sum _; do
    case "$package" in '' | '#'*) continue ;; esac
    if grep -qF "sha256:$sum" "$df"; then
      needs+=("file $package $version $name")
    fi
  done < upstream-files.txt
  if grep -qF generic/go-modules/ "$df"; then
    needs+=("file go-modules $(sha go.sum) go-modules.tar.gz")
  fi
  if grep -qF generic/npm-cache/ "$df"; then
    needs+=("file npm-cache $(sha opencode-plugin/package-lock.json) npm-cache.tar.gz")
  fi
done
[ "${#needs[@]}" -gt 0 ] || exit 0
mapfile -t needs < <(printf '%s\n' "${needs[@]}" | sort -u)
# held image <name> <tag> <digest> | held file <package> <version> <file>
held() {
  if [ "$1" = file ]; then
    curl -fsS -o /dev/null -I "$store/api/packages/upstream/generic/$2/$3/$4"
    return
  fi
  local token
  token=$(curl -fsS "$store/v2/token?scope=repository:upstream/$2:pull" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
  curl -fsS -o /dev/null -I -H "Authorization: Bearer $token" "$store/v2/upstream/$2/manifests/$4"
}
deadline=$((SECONDS + wait))
while :; do
  missing=()
  for n in "${needs[@]}"; do
    held $n 2>/dev/null || missing+=("${n#* }")
  done
  if [ "${#missing[@]}" -eq 0 ]; then
    echo "the store holds the ${#needs[@]} inputs $* read"
    exit 0
  fi
  [ "$SECONDS" -lt "$deadline" ] || break
  echo "the store lacks ${#missing[@]} of the ${#needs[@]} inputs $* read: waiting for the copy before the checks"
  sleep 30
done
for m in "${missing[@]}"; do
  echo "the store lacks $m: the copy before the checks did not make it"
done
exit 1
