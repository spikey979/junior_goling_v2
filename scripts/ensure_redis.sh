#!/usr/bin/env bash
set -euo pipefail

# Ensures a local Redis is running on 127.0.0.1:6379.
# If not, starts a docker container named redis-local.

REDIS_HOST="127.0.0.1"
REDIS_PORT=6379
CONTAINER_NAME="redis-local"
IMAGE="redis:7-alpine"

is_port_open() {
  (echo > /dev/tcp/${REDIS_HOST}/${REDIS_PORT}) >/dev/null 2>&1
}

if is_port_open; then
  echo "Redis is already running on ${REDIS_HOST}:${REDIS_PORT}"
  exit 0
fi

echo "Redis not detected on ${REDIS_HOST}:${REDIS_PORT}. Checking Docker..."
if ! command -v docker >/dev/null 2>&1; then
  echo "Docker not found. Please install Docker or start Redis manually." >&2
  exit 1
fi

if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
  echo "Starting existing container ${CONTAINER_NAME}..."
  docker start "${CONTAINER_NAME}" >/dev/null
elif docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
  echo "Starting existing container ${CONTAINER_NAME}..."
  docker start "${CONTAINER_NAME}" >/dev/null
else
  echo "Running new Redis container ${CONTAINER_NAME} from ${IMAGE}..."
  docker run -d --name "${CONTAINER_NAME}" -p ${REDIS_PORT}:6379 "${IMAGE}" >/dev/null
fi

echo "Waiting for Redis to become ready..."
for i in {1..20}; do
  if is_port_open; then
    echo "Redis is up on ${REDIS_HOST}:${REDIS_PORT}"
    exit 0
  fi
  sleep 0.5
done

echo "Redis did not become ready in time." >&2
exit 2

