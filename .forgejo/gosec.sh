#!/usr/bin/env bash
# gosec over the packages `go list ./...` names in the module of the
# working directory: the one way pr.yml's security job, main's CI and
# `bash check` run it. It lives in .forgejo, which no agent branch may
# change, since it runs every pull request's scan and main's. The
# arguments are the gosec command: the runner image's binary, or a go run
# of its version.
#
# go/packages hands gosec a package as the absolute paths of its files and
# splits a list past 16383 characters into separate go list calls, each its
# own package: each part misses the others' names, so gosec reports type
# errors and skips the package's analysis. gosec therefore runs from a
# short link to the checkout, made for the run, and a package whose list
# would still pass the limit there is refused before it runs. gosec runs
# without -quiet, so those errors fail every run; its line per checked
# file is left out.
set -euo pipefail
: "${1:?usage: .forgejo/gosec.sh <gosec command>, run from the module root}"
root=$(pwd -P)
tmp=$(mktemp -d /tmp/gosec.XXXXXX)
trap 'rm -rf "$tmp"' EXIT
ln -s "$root" "$tmp/s"
cd "$tmp/s"
# each package: its import path, its directory and the paths of the files
# gosec checks
pkgs=$(go list -f '{{$d := .Dir}}{{.ImportPath}} {{.Dir}}{{range .GoFiles}} {{$d}}/{{.}}{{end}}{{range .CgoFiles}} {{$d}}/{{.}}{{end}}' ./...)
over=$(awk '{ n = 0; for (i = 3; i <= NF; i++) n += length($i) + 1; if (n > 16383) print $1, n }' <<<"$pkgs")
if [ -n "$over" ]; then
  while read -r pkg n; do
    echo "package $pkg lists $n characters of file paths from $tmp/s, past go/packages' 16383: gosec would type-check it in parts. Split the package." >&2
  done <<<"$over"
  exit 1
fi
mapfile -t dirs < <(awk 'NF > 2 { print $2 }' <<<"$pkgs")
{ "$@" "${dirs[@]}" 2>&1 1>&3 3>&- | { grep --line-buffered -v '^\[gosec\] .* Checking file: ' || true; } >&2; } 3>&1
