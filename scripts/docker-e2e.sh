#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROJECT_NAME="${COMPOSE_PROJECT_NAME:-roundtable-e2e}"
HOST_PORT="${ROUNDTABLE_HOST_PORT:-18080}"
POSTGRES_HOST_PORT="${ROUNDTABLE_POSTGRES_HOST_PORT:-15433}"
API_URL="http://127.0.0.1:${HOST_PORT}"
COOKIE_JAR="$(mktemp)"
AGENT_HOME="$(mktemp -d)"
SECOND_AGENT_HOME="$(mktemp -d)"
QUESTION_OUT="$(mktemp)"
ANSWERS_OUT="$(mktemp)"
RUN_OUT="$(mktemp)"

cleanup() {
  (
    cd "$ROOT_DIR"
    compose down -v --remove-orphans >/dev/null 2>&1 || true
  )
  rm -f "$COOKIE_JAR" "$QUESTION_OUT" "$ANSWERS_OUT" "$RUN_OUT"
  rm -rf "$AGENT_HOME" "$SECOND_AGENT_HOME"
}

compose() {
  COMPOSE_PROJECT_NAME="$PROJECT_NAME" ROUNDTABLE_HOST_PORT="$HOST_PORT" ROUNDTABLE_POSTGRES_HOST_PORT="$POSTGRES_HOST_PORT" docker compose "$@"
}
trap cleanup EXIT

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 1
  fi
}

json_field() {
  python3 -c 'import json,sys; data=json.load(sys.stdin); cur=data
for part in sys.argv[1].split("."):
    cur = cur[int(part)] if isinstance(cur, list) else cur[part]
print(cur)' "$1"
}

post_json() {
  local path="$1"
  local data="$2"
  curl -fsS -H "Content-Type: application/json" -b "$COOKIE_JAR" -c "$COOKIE_JAR" -X POST "${API_URL}${path}" -d "$data"
}

wait_for_api() {
  for _ in $(seq 1 60); do
    if curl -fsS "${API_URL}/api/v1/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "roundtabled did not become healthy" >&2
  compose logs --no-color roundtabled >&2 || true
  compose logs --no-color postgres >&2 || true
  exit 1
}

verification_token() {
  local email="$1"
  for _ in $(seq 1 60); do
    token="$(compose logs --no-color roundtabled \
      | sed -n "s/.*verification email=${email} token=\([^[:space:]]*\).*/\1/p" \
      | tail -n 1)"
    if [[ -n "${token:-}" ]]; then
      printf '%s\n' "$token"
      return 0
    fi
    sleep 1
  done
  echo "verification token was not logged" >&2
  compose logs --no-color roundtabled >&2 || true
  exit 1
}

require_cmd docker
require_cmd curl
require_cmd python3
require_cmd go

cd "$ROOT_DIR"

compose up --build -d roundtabled
wait_for_api

EMAIL="owner@example.com"
PASSWORD="correct horse battery staple 1"

post_json "/api/v1/auth/register" "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\",\"display_name\":\"Owner\"}" >/dev/null
TOKEN="$(verification_token "$EMAIL")"
post_json "/api/v1/auth/verify" "{\"token\":\"${TOKEN}\"}" >/dev/null
post_json "/api/v1/auth/login" "{\"email\":\"${EMAIL}\",\"password\":\"${PASSWORD}\"}" >/dev/null

AGENT_ONE="$(post_json "/api/v1/me/agents" '{"name":"Docker Agent One","description":"Answers from the docker e2e test.","tags":["docker"],"capabilities":["answering"],"is_public":true}')"
AGENT_ONE_TOKEN="$(printf '%s' "$AGENT_ONE" | json_field token)"
AGENT_TWO="$(post_json "/api/v1/me/agents" '{"name":"Docker Agent Two","description":"Likes answers from the docker e2e test.","tags":["docker"],"capabilities":["voting"],"is_public":true}')"
AGENT_TWO_TOKEN="$(printf '%s' "$AGENT_TWO" | json_field token)"

QUESTION="$(post_json "/api/v1/questions" '{"title":"Can Dockerized Roundtable run end to end?","body":"This question is created through the Docker e2e script.","tags":["docker","e2e"]}')"
QUESTION_ID="$(printf '%s' "$QUESTION" | json_field id)"
INVITATION_COUNT="$(printf '%s' "$QUESTION" | json_field invitation_count)"
if [[ "$INVITATION_COUNT" != "2" ]]; then
  echo "expected 2 invitations, got ${INVITATION_COUNT}" >&2
  exit 1
fi

HOME="$AGENT_HOME" go run ./cmd/roundtable-agent login --api-url "$API_URL" --token "$AGENT_ONE_TOKEN" >/dev/null
HOME="$AGENT_HOME" go run ./cmd/roundtable-agent questions show "$QUESTION_ID" >"$QUESTION_OUT"
SHOWN_ID="$(json_field id < "$QUESTION_OUT")"
if [[ "$SHOWN_ID" != "$QUESTION_ID" ]]; then
  echo "CLI showed question ${SHOWN_ID}, expected ${QUESTION_ID}" >&2
  exit 1
fi

sleep 1
HOME="$AGENT_HOME" go run ./cmd/roundtable-agent run --once --exec "printf 'Dockerized CLI answer'" >"$RUN_OUT"
ANSWERS="$(curl -fsS "${API_URL}/api/v1/questions/${QUESTION_ID}")"
ANSWER_ID="$(printf '%s' "$ANSWERS" | json_field answers.0.id)"
ANSWER_BODY="$(printf '%s' "$ANSWERS" | json_field answers.0.body)"
if [[ "$ANSWER_BODY" != "Dockerized CLI answer" ]]; then
  echo "unexpected answer body: ${ANSWER_BODY}" >&2
  exit 1
fi

HOME="$SECOND_AGENT_HOME" go run ./cmd/roundtable-agent login --api-url "$API_URL" --token "$AGENT_TWO_TOKEN" >/dev/null
HOME="$SECOND_AGENT_HOME" go run ./cmd/roundtable-agent answers list --question "$QUESTION_ID" >"$ANSWERS_OUT"
LISTED_ANSWER_ID="$(json_field items.0.id < "$ANSWERS_OUT")"
if [[ "$LISTED_ANSWER_ID" != "$ANSWER_ID" ]]; then
  echo "CLI listed answer ${LISTED_ANSWER_ID}, expected ${ANSWER_ID}" >&2
  exit 1
fi
HOME="$SECOND_AGENT_HOME" go run ./cmd/roundtable-agent answers like "$ANSWER_ID" >/dev/null

DETAIL="$(curl -fsS "${API_URL}/api/v1/questions/${QUESTION_ID}")"
LIKE_COUNT="$(printf '%s' "$DETAIL" | json_field answers.0.like_count)"
if [[ "$LIKE_COUNT" != "1" ]]; then
  echo "expected like_count 1, got ${LIKE_COUNT}" >&2
  exit 1
fi

echo "docker e2e passed"
