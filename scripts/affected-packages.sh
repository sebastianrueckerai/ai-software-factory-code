#!/usr/bin/env bash
# scripts/affected-packages.sh <base> prints the Go packages a change since
# base reaches, on one line, or ./... for the whole suite; why goes to
# stderr. The change is the work tree's, untracked files included: a
# sandbox's uncommitted work counts, and a CI checkout's work tree is its
# HEAD. pr.yml runs the base's copy, bash check changed the work tree's.
# One rule covers every input the package walk cannot see — workflows,
# scripts, Dockerfiles, migrations, testdata, go.mod: any non-Go file in
# the diff selects the whole suite, as does any surprise in the walk
# Fail open to more testing, never less. One exception: a diff confined
# to k8s/ selects ./internal/reproducible/... only, the package whose
# tests read the pins and the manifests; a roll cut from a green main
# need not re-run the whole suite for a manifest tweak.
set -euo pipefail
base=${1:?usage: $0 <base>}
cd "$(git rev-parse --show-toplevel)"
full() { echo "$1: full suite" >&2; echo "./..."; exit 0; }
changed=$({ git diff --name-only "$base"; git ls-files --others --exclude-standard; } | sort -u) || full "cannot diff against $base"
echo "changed files:" >&2
echo "$changed" >&2
[ -n "$changed" ] || full "empty diff"
if ! echo "$changed" | grep -qv '^k8s/'; then
  echo "a diff confined to k8s/: the reproducible package" >&2
  echo "./internal/reproducible/..."
  exit 0
fi
if echo "$changed" | grep -qvE '\.go$'; then full "a non-Go input changed"; fi
mapfile -t dirs < <(echo "$changed" | while IFS= read -r f; do dirname "$f"; done | sort -u)
for d in "${dirs[@]}"; do
  compgen -G "$d/*.go" >/dev/null || full "$d holds no Go files"
done
changed_pkgs=$(go list "${dirs[@]/#/.\/}" 2>/dev/null) || full "go list failed on a changed directory"
pkgs=$(
  go list -test -f '{{.ImportPath}} {{join .Deps " "}}' ./... 2>/dev/null |
    awk -v changed="$changed_pkgs" '
      BEGIN { n = split(changed, c, " "); for (i = 1; i <= n; i++) want[c[i]] = 1 }
      $1 ~ /\.test$/ {
        p = substr($1, 1, length($1) - 5)
        for (i = 2; i <= NF; i++) if ($i in want) { print p; break }
      }
      END { for (p in want) print p }
    ' | sort -u
) || pkgs=""
[ -n "$pkgs" ] || full "the walk named no package"
pkgs=$(printf '%s\n' "$pkgs" | paste -sd' ')
echo "packages to test: $pkgs" >&2
echo "$pkgs"
