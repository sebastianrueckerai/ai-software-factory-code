#!/usr/bin/env bash
# roll.sh staging|prod DIGEST: factory-ci copies deploy/<env> and deploy/base
# of the commit this run built (GITHUB_SHA) onto the deploy branch as a
# self-contained deploy/<env> (its base inside it), sets DIGEST there, leaves
# the other environment untouched, and pushes deploy over Forgejo's SSH with
# the deploy key. So a base change reaches prod only with prod's roll. The deploy branch
# holds deploy/ only, so its protected-file pattern (.forgejo/**) never
# meets a change; Flux tracks it. main is never pushed here, and the deploy
# key cannot push it.
# The host key is pinned: the one ssh-keyscan fetches must equal the
# Actions secret FACTORY_SSH_HOST_KEY, which registration writes.
# Registration seeds this file without the executable bit (the contents
# API writes mode 100644): ci.yml calls it as bash .forgejo/roll.sh. The
# image is named by GIT_SSH_REPO, the project's repository.
set -euo pipefail
env=$1 digest=$2
: "${DEPLOY_KEY:?}" "${FACTORY_SSH_HOST_KEY:?}" "${GIT_SSH_HOST:?}" "${GIT_SSH_PORT:?}" "${GIT_SSH_REPO:?}" "${GITHUB_SHA:?}"
echo "$digest" | grep -qxE 'sha256:[0-9a-f]{64}' || { echo "not an image digest: $digest"; exit 1; }
file=deploy/$env/kustomization.yaml
image=${IMAGE:-$GIT_SSH_REPO}

# the secret may hold a bare key, a known_hosts or an authorized_keys line
pinned=$(printf '%s\n' "$FACTORY_SSH_HOST_KEY" | awk '{for (i = 1; i < NF; i++) if ($i ~ /^(ssh-(ed25519|rsa)|ecdsa-sha2-nistp(256|384|521))$/) {print $i, $(i+1); exit}}')
[ -n "$pinned" ] || { echo "FACTORY_SSH_HOST_KEY holds no SSH host key"; exit 1; }
tmp=$(mktemp -d)
scanned=$(ssh-keyscan -T 10 -t "${pinned%% *}" -p "$GIT_SSH_PORT" "$GIT_SSH_HOST" 2>/dev/null | awk -v t="${pinned%% *}" '$2 == t {print $2, $3}')
if [ "$scanned" != "$pinned" ]; then
  echo "Forgejo's SSH host key at $GIT_SSH_HOST:$GIT_SSH_PORT is not the pinned one:"
  echo "  pinned:  $pinned"
  echo "  scanned: ${scanned:-nothing}"
  exit 1
fi
known=$GIT_SSH_HOST
[ "$GIT_SSH_PORT" = 22 ] || known="[$GIT_SSH_HOST]:$GIT_SSH_PORT"
echo "$known $pinned" > "$tmp/known_hosts"
eval "$(ssh-agent -s)" >/dev/null
trap 'ssh-agent -k >/dev/null' EXIT
printf '%s\n' "$DEPLOY_KEY" | ssh-add -q -
export GIT_SSH_COMMAND="ssh -o UserKnownHostsFile=$tmp/known_hosts -o StrictHostKeyChecking=yes -o BatchMode=yes"
url="ssh://git@$GIT_SSH_HOST:$GIT_SSH_PORT/$GIT_SSH_REPO.git"

src=$PWD
d=$tmp/deploy
git init -q "$d"
git -C "$d" config user.name factory-ci
git -C "$d" config user.email factory-ci@aisoftwarefactory.world
digest_of() { sed -nE 's/^ *digest: (sha256:[0-9a-f]{64})$/\1/p' "$d/deploy/$1/kustomization.yaml" 2>/dev/null || true; }
set_digest() { sed -i -E "s/^( *digest: )sha256:[0-9a-f]{64}$/\1$2/" "$d/deploy/$1/kustomization.yaml"; }
for attempt in 1 2 3; do
  git -C "$d" fetch -q "$url" main
  if [ "$env" = staging ] && [ "$(git -C "$d" rev-list --count "$GITHUB_SHA..FETCH_HEAD" 2>/dev/null)" != 0 ]; then
    echo "main moved past ${GITHUB_SHA::7}; its own run rolls staging"; exit 0
  fi
  if git -C "$d" fetch -q "$url" deploy 2>/dev/null; then
    git -C "$d" checkout -q -f -B deploy FETCH_HEAD
    base=$(git -C "$d" rev-parse HEAD)
  else
    echo "no deploy branch yet: starting it"
    git -C "$d" update-ref -d refs/heads/deploy 2>/dev/null || true
    git -C "$d" symbolic-ref HEAD refs/heads/deploy
    git -C "$d" read-tree --empty
    base=""
  fi
  if [ "$env" = prod ] && [ "$(digest_of staging)" != "$digest" ]; then
    echo "staging no longer runs $digest; the run that rolled it on promotes"; exit 0
  fi
  rm -rf "$tmp/src" "$d/deploy/$env"
  mkdir -p "$tmp/src" "$d/deploy"
  git -C "$src" archive "$GITHUB_SHA" "deploy/$env" deploy/base | tar -x -C "$tmp/src"
  mv "$tmp/src/deploy/$env" "$d/deploy/$env"
  mv "$tmp/src/deploy/base" "$d/deploy/$env/base"
  sed -i -E 's#^resources: \[\.\./base\]$#resources: [base]#' "$d/deploy/$env/kustomization.yaml"
  grep -qx 'resources: \[base\]' "$d/deploy/$env/kustomization.yaml" || { echo "deploy/$env/kustomization.yaml does not read 'resources: [../base]'"; exit 1; }
  set_digest "$env" "$digest"
  # the shared base goes once no environment reads it any more
  grep -qs '\.\./base' "$d"/deploy/*/kustomization.yaml || rm -rf "$d/deploy/base"
  git -C "$d" add -A deploy
  if [ -n "$base" ] && git -C "$d" diff --cached --quiet; then echo "$env already runs $digest"; exit 0; fi
  git -C "$d" commit -qm "Roll $env to ${GITHUB_SHA::7}" -m "deploy/$env of main ${GITHUB_SHA}; $image@$digest${ROLL_NOTE:+, $ROLL_NOTE}"
  if git -C "$d" push -q "$url" HEAD:refs/heads/deploy; then echo "$env rolled to $digest on deploy"; exit 0; fi
done
echo "push to deploy refused three times"; exit 1
