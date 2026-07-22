#!/usr/bin/env bash
# Starts (or restarts) a socat sidecar that proxies TCP from the Docker
# bridge gateway IP to a service bound to the host's own 127.0.0.1 (e.g. a
# MongoDB install whose bindIp you're not allowed to change). Same idea as
# mysql-host-proxy.sh in this folder, just with MongoDB's default port.
# Safe to re-run any number of times — it replaces the container in place
# instead of erroring if one already exists. See README.md in this folder
# for when/why this is needed and what to set in the admin UI afterwards.
set -euo pipefail

CONTAINER_NAME="${CONTAINER_NAME:-mongo-host-proxy}"
BIND_IP="${BIND_IP:-$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')}"
LISTEN_PORT="${LISTEN_PORT:-37017}"
TARGET_HOST="${TARGET_HOST:-127.0.0.1}"
TARGET_PORT="${TARGET_PORT:-27017}"

echo "==> Proxying ${BIND_IP}:${LISTEN_PORT} -> ${TARGET_HOST}:${TARGET_PORT}"

if docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
  echo "==> Removing existing $CONTAINER_NAME container"
  docker rm -f "$CONTAINER_NAME" >/dev/null
fi

docker run -d --name "$CONTAINER_NAME" \
  --network host \
  --restart unless-stopped \
  alpine/socat \
  "tcp-listen:${LISTEN_PORT},fork,reuseaddr,bind=${BIND_IP}" "tcp-connect:${TARGET_HOST}:${TARGET_PORT}"

echo "==> Done. In BackupDB admin UI, set host=host.docker.internal, port=${LISTEN_PORT}"
