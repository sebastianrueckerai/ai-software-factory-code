#!/usr/bin/env bash
# ci-volume-use.sh: one line with the disk use, in MiB, of each volume of a CI
# runner pod, for sizing their sizeLimits from a week of job logs. A
# directory that does not exist reads 0. CI_VOLUME_ROOT prefixes every path
# (the test's own tree); it is empty in a job.
r=${CI_VOLUME_ROOT:-}
use() { [ -d "$r$1" ] && du -sm "$r$1" 2>/dev/null | cut -f1 || echo 0; }
echo "ci-volume-use workspace=$(use /workspace) home=$(use /home/runner) tmp=$(use /tmp) modcache=$(use /modcache) cache=$(use /cache)"
