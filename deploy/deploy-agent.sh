#!/usr/bin/env bash
# Builds the backupdb Docker image locally and ships it to a remote `agent`
# server via docker save/scp/docker load — same idea as deploy.sh, but
# starts the `agent` service from docker-compose.agent.yml instead of the
# main docker-compose.yml. See README.md in this folder, "Deploy agent",
# for the full walkthrough (first-time setup, firewall, registering the
# agent's fingerprint in the central admin UI).
set -euo pipefail

: "${DEPLOY_HOST:?Set DEPLOY_HOST, e.g. DEPLOY_HOST=user@agent-server}"
: "${DEPLOY_PATH:?Set DEPLOY_PATH, the checkout path on the agent server, e.g. /home/user/backup-db-go-agent}"

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

echo "==> Uploading to $DEPLOY_HOST"
REMOTE_TAR="$(ssh "$DEPLOY_HOST" mktemp --suffix=.tar.gz)"
scp "$LOCAL_TAR" "$DEPLOY_HOST:$REMOTE_TAR"

echo "==> Loading image and restarting the agent on $DEPLOY_HOST"
ssh "$DEPLOY_HOST" "
  set -e
  docker load -i '$REMOTE_TAR'
  rm -f '$REMOTE_TAR'
  cd '$DEPLOY_PATH'
  docker compose -f docker-compose.agent.yml up -d
  docker image prune -f >/dev/null
"

echo "==> Done. First deploy? Grab the certificate fingerprint to register in the central admin UI:"
echo "    ssh $DEPLOY_HOST \"cd $DEPLOY_PATH && docker compose -f docker-compose.agent.yml logs agent\" | grep fingerprint"
