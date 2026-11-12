#!/usr/bin/env bash
# ci-volume-use.sh: one line with the disk use, in MiB, of each volume of a CI
# runner pod, for sizing their sizeLimits from a week of job logs. A
# directory that does not exist reads 0, and a read that fails or passes
# its 30 s bound reads ?. cache is the project's whole shared claim on the
# node, not this job's share. CI_VOLUME_ROOT prefixes every path (the
# test's own tree); it is empty in a job.
r=${CI_VOLUME_ROOT:-}
use() {
  [ -d "$r$1" ] || { echo 0; return; }
  local n
  n=$(timeout 30 du -sm "$r$1" 2>/dev/null | cut -f1)
  echo "${n:-?}"
}
echo "ci-volume-use workspace=$(use /workspace) home=$(use /home/runner) tmp=$(use /tmp) modcache=$(use /modcache) cache=$(use /cache)"
