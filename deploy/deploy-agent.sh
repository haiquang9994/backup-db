#!/usr/bin/env bash
# Builds the backupdb Docker image locally and ships it to one or more
# remote `agent` servers via docker save/scp/docker load — same idea as
# deploy.sh, but starts the `agent` service from docker-compose.agent.yml
# instead of the main docker-compose.yml. See README.md in this folder,
# "Deploy agent", for the full walkthrough (first-time setup, firewall,
# registering the agent's fingerprint in the central admin UI).
#
# The image is built and saved to a tarball exactly once no matter how many
# targets are deployed to — only the per-host upload+load (unavoidable,
# each host needs its own copy of the tarball) repeats.
set -euo pipefail

# DEPLOY_TARGETS: space-separated "host:path" pairs, e.g.
#   DEPLOY_TARGETS="root@a2:/root/backupdb root@oss:/root/backupdb"
# for deploying the same build to several agent servers in one run. Falls
# back to a single DEPLOY_HOST/DEPLOY_PATH pair for one-off single-agent
# deploys.
if [ -n "${DEPLOY_TARGETS:-}" ]; then
  TARGETS="$DEPLOY_TARGETS"
else
  : "${DEPLOY_HOST:?Set DEPLOY_HOST, e.g. DEPLOY_HOST=user@agent-server (or DEPLOY_TARGETS to deploy to several hosts at once)}"
  : "${DEPLOY_PATH:?Set DEPLOY_PATH, the checkout path on the agent server, e.g. /home/user/backup-db-go-agent}"
  TARGETS="$DEPLOY_HOST:$DEPLOY_PATH"
fi

PLATFORM="${DOCKER_PLATFORM:-}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Building backupdb:latest from $REPO_ROOT"
if [ -n "$PLATFORM" ]; then
  docker build --platform "$PLATFORM" -t backupdb:latest "$REPO_ROOT"
else
  docker build -t backupdb:latest "$REPO_ROOT"
fi

LOCAL_TAR="$(mktemp --suffix=.tar.gz)"
trap 'rm -f "$LOCAL_TAR"' EXIT

echo "==> Saving image to $LOCAL_TAR"
docker save backupdb:latest | gzip > "$LOCAL_TAR"

echo "==> Pruning dangling images left over from previous builds (local)"
docker image prune -f >/dev/null

for target in $TARGETS; do
  host="${target%%:*}"
  path="${target#*:}"

  echo "==> Uploading to $host"
  remote_tar="$(ssh "$host" mktemp --suffix=.tar.gz)"
  scp "$LOCAL_TAR" "$host:$remote_tar"

  echo "==> Loading image and restarting the agent on $host"
  ssh "$host" "
    set -e
    docker load -i '$remote_tar'
    rm -f '$remote_tar'
    cd '$path'
    docker compose -f docker-compose.agent.yml up -d
    docker image prune -f >/dev/null
  "

  echo "==> Done for $host. First deploy? Grab the certificate fingerprint to register in the central admin UI:"
  echo "    ssh $host \"cd $path && docker compose -f docker-compose.agent.yml logs agent\" | grep fingerprint"
done
