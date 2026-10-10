#!/usr/bin/env bash
set -Eeuo pipefail
cd "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"

if ! command -v docker >/dev/null 2>&1; then
  printf 'Docker is required. Install Docker Desktop, start it, and run ./start.sh again.\n' >&2
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  printf 'Docker Compose is required. Update Docker Desktop and try again.\n' >&2
  exit 1
fi

case "${1:-}" in
  --stop) docker compose down; exit 0 ;;
  --logs) docker compose logs --follow; exit 0 ;;
  "") ;;
  *) printf 'Usage: ./start.sh [--stop | --logs]\n' >&2; exit 1 ;;
esac

if ! docker info >/dev/null 2>&1; then
  printf 'Docker is not running. Start Docker Desktop and run ./start.sh again.\n' >&2
  exit 1
fi
if [[ ! -f .env && -z "${DEEPSEEK_API_KEY:-}" ]]; then
  printf 'Missing .env. Copy .env.example to .env and add your DeepSeek API key.\n' >&2
  exit 1
fi
docker compose config --quiet
printf '\nStarting Harper…\n\n'
docker compose up --build --detach --wait --wait-timeout 120
printf '\nHarper is ready: http://localhost:4003\nBackend:         http://localhost:4004/api/health\n\nFirst visit: create your owner account and save the recovery key.\n\nStop: ./start.sh --stop\nLogs: ./start.sh --logs\n'
