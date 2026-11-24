#!/usr/bin/env bash
# gosec over every package of the module in the working directory: the one
# way pr.yml's security job, main's CI and `bash check` run it. The
# arguments are the gosec command: the runner image's binary, or a go run
# of its version.
#
# go/packages hands gosec a package as the absolute paths of its files and
# splits a list past 16383 characters into separate go list calls, each its
# own package: each part misses the others' names, so gosec reports type
# errors and skips the package's analysis. gosec therefore runs from a
# short link to the checkout, and a package whose list would still pass the
# limit there is refused before it runs. gosec runs without -quiet, so
# those errors fail every run; its line per checked file is left out.
set -euo pipefail
: "${1:?usage: scripts/gosec.sh <gosec command>, run from the module root}"
limit=${GOSEC_LIST_MAX:-16383}
root=$(pwd -P)
link=/tmp/gosec-$$
ln -sfn "$root" "$link"
trap 'rm -f "$link"' EXIT
cd "$link"
over=$(go list -f '{{$d := .Dir}}{{.ImportPath}}{{range .GoFiles}} {{$d}}/{{.}}{{end}}{{range .CgoFiles}} {{$d}}/{{.}}{{end}}' ./... |
  awk -v max="$limit" '{ n = 0; for (i = 2; i <= NF; i++) n += length($i) + 1; if (n > max) print $1, n }')
if [ -n "$over" ]; then
  while read -r pkg n; do
    echo "package $pkg lists $n characters of file paths from $link, past go/packages' $limit: gosec would type-check it in parts. Split the package." >&2
  done <<<"$over"
  exit 1
fi
{ "$@" ./... 2>&1 1>&3 3>&- | { grep -v '^\[gosec\] .* Checking file: ' || true; } >&2; } 3>&1
