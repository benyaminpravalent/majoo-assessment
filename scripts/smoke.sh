#!/usr/bin/env bash
#
# End-to-end smoke test against a running API.
#
#   make up && ./scripts/smoke.sh
#   BASE_URL=https://staging.example.com ./scripts/smoke.sh
#
# It walks the reviewer flow from the README — register, log in, create a post,
# read it, comment, and confirm that a second account cannot modify either — and
# asserts the status code at every step. Anything unexpected fails the script.
#
# Requires curl and jq.

set -euo pipefail

BASE_URL="${BASE_URL:-http://localhost:8080}"
API="${BASE_URL}/api/v1"

# A random suffix so the script can be run repeatedly against the same database
# without colliding with its own earlier accounts.
SUFFIX="$(date +%s)$RANDOM"
ALICE_EMAIL="alice+${SUFFIX}@example.com"
ALICE_USER="alice${SUFFIX}"
MALLORY_EMAIL="mallory+${SUFFIX}@example.com"
MALLORY_USER="mallory${SUFFIX}"
PASSWORD='a-good-enough-password'

if [ -t 1 ]; then
  GREEN=$'\033[32m'; RED=$'\033[31m'; DIM=$'\033[2m'; RESET=$'\033[0m'
else
  GREEN=''; RED=''; DIM=''; RESET=''
fi

PASSED=0
FAILED=0

for tool in curl jq; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "${RED}error:${RESET} $tool is required but not installed" >&2
    exit 1
  fi
done

# request METHOD PATH [BODY] [TOKEN]
# Writes the response body to $BODY and the status code to $STATUS.
request() {
  local method="$1" path="$2" body="${3:-}" token="${4:-}"
  local args=(-sS -o /tmp/smoke_body.$$ -w '%{http_code}' -X "$method" "${API}${path}")

  [ -n "$body" ] && args+=(-H 'Content-Type: application/json' -d "$body")
  [ -n "$token" ] && args+=(-H "Authorization: Bearer ${token}")

  STATUS="$(curl "${args[@]}")"
  BODY="$(cat /tmp/smoke_body.$$)"
  rm -f /tmp/smoke_body.$$
}

# expect EXPECTED_STATUS DESCRIPTION
expect() {
  local want="$1" what="$2"
  if [ "$STATUS" = "$want" ]; then
    printf '  %s✓%s %-58s %s\n' "$GREEN" "$RESET" "$what" "$STATUS"
    PASSED=$((PASSED + 1))
  else
    printf '  %s✗%s %-58s %s (wanted %s)\n' "$RED" "$RESET" "$what" "$STATUS" "$want"
    printf '    %s%s%s\n' "$DIM" "$BODY" "$RESET"
    FAILED=$((FAILED + 1))
  fi
}

echo
echo "Smoke test against ${BASE_URL}"
echo

# ---------------------------------------------------------------------------
echo "Health and metadata"
# ---------------------------------------------------------------------------
STATUS="$(curl -sS -o /tmp/smoke_body.$$ -w '%{http_code}' "${BASE_URL}/healthz")"
BODY="$(cat /tmp/smoke_body.$$)"; rm -f /tmp/smoke_body.$$
expect 200 "GET /healthz is alive"

STATUS="$(curl -sS -o /tmp/smoke_body.$$ -w '%{http_code}' "${BASE_URL}/readyz")"
BODY="$(cat /tmp/smoke_body.$$)"; rm -f /tmp/smoke_body.$$
expect 200 "GET /readyz can reach the database"

STATUS="$(curl -sS -o /tmp/smoke_body.$$ -w '%{http_code}' "${BASE_URL}/openapi.yaml")"
BODY="$(cat /tmp/smoke_body.$$)"; rm -f /tmp/smoke_body.$$
expect 200 "GET /openapi.yaml serves the contract"

# ---------------------------------------------------------------------------
echo
echo "Registration and authentication"
# ---------------------------------------------------------------------------
request POST /auth/register \
  "{\"email\":\"${ALICE_EMAIL}\",\"username\":\"${ALICE_USER}\",\"display_name\":\"Alice\",\"password\":\"${PASSWORD}\"}"
expect 201 "register alice"

request POST /auth/register \
  "{\"email\":\"${ALICE_EMAIL}\",\"username\":\"${ALICE_USER}x\",\"display_name\":\"Alice\",\"password\":\"${PASSWORD}\"}"
expect 409 "duplicate email is rejected"

request POST /auth/register '{"email":"not-an-email","username":"x","display_name":"","password":"short"}'
expect 422 "invalid registration reports field errors"
echo "    ${DIM}$(echo "$BODY" | jq -c '.error.fields // []')${RESET}"

request POST /auth/register \
  "{\"email\":\"${MALLORY_EMAIL}\",\"username\":\"${MALLORY_USER}\",\"display_name\":\"Mallory\",\"password\":\"${PASSWORD}\"}"
expect 201 "register mallory"

request POST /auth/login "{\"email\":\"${ALICE_EMAIL}\",\"password\":\"wrong-password\"}"
expect 401 "wrong password is rejected"

request POST /auth/login "{\"email\":\"nobody-${SUFFIX}@example.com\",\"password\":\"${PASSWORD}\"}"
expect 401 "unknown account is rejected identically"

request POST /auth/login "{\"email\":\"${ALICE_EMAIL}\",\"password\":\"${PASSWORD}\"}"
expect 200 "login alice"
ALICE_TOKEN="$(echo "$BODY" | jq -r '.data.access_token')"
ALICE_REFRESH="$(echo "$BODY" | jq -r '.data.refresh_token')"

request POST /auth/login "{\"email\":\"${MALLORY_EMAIL}\",\"password\":\"${PASSWORD}\"}"
expect 200 "login mallory"
MALLORY_TOKEN="$(echo "$BODY" | jq -r '.data.access_token')"

request GET /users/me '' "$ALICE_TOKEN"
expect 200 "GET /users/me with a valid token"

request GET /users/me
expect 401 "GET /users/me without a token"

request GET /users/me '' 'not.a.real.token'
expect 401 "GET /users/me with a forged token"

# ---------------------------------------------------------------------------
echo
echo "Posts"
# ---------------------------------------------------------------------------
request POST /posts \
  '{"title":"Smoke Test Post","content":"Written by scripts/smoke.sh.","status":"published"}' \
  "$ALICE_TOKEN"
expect 201 "alice creates a published post"
POST_ID="$(echo "$BODY" | jq -r '.data.id')"

request POST /posts '{"title":"Anonymous Post","content":"body"}'
expect 401 "anonymous post creation is rejected"

request POST /posts '{"title":"ab","content":""}' "$ALICE_TOKEN"
expect 422 "invalid post is rejected with field errors"

request GET "/posts/${POST_ID}"
expect 200 "anyone can read the published post"

request GET '/posts?limit=5'
expect 200 "listing posts returns pagination metadata"
echo "    ${DIM}$(echo "$BODY" | jq -c '.pagination')${RESET}"

request GET '/posts?page=0'
expect 422 "page=0 is a validation error, not a silent default"

request GET '/posts?limit=1000'
expect 422 "limit above the maximum is rejected"

request POST /posts '{"title":"Alice Draft","content":"unpublished"}' "$ALICE_TOKEN"
expect 201 "alice creates a draft"
DRAFT_ID="$(echo "$BODY" | jq -r '.data.id')"

request GET "/posts/${DRAFT_ID}"
expect 404 "the draft is invisible to anonymous callers"

request GET "/posts/${DRAFT_ID}" '' "$MALLORY_TOKEN"
expect 404 "the draft is invisible to another user — 404, not 403"

request GET "/posts/${DRAFT_ID}" '' "$ALICE_TOKEN"
expect 200 "the draft is visible to its author"

request PATCH "/posts/${POST_ID}" '{"title":"Hijacked Title"}' "$MALLORY_TOKEN"
expect 403 "mallory cannot edit alice's post"

request DELETE "/posts/${POST_ID}" '' "$MALLORY_TOKEN"
expect 403 "mallory cannot delete alice's post"

request PATCH "/posts/${POST_ID}" '{"title":"Smoke Test Post, revised"}' "$ALICE_TOKEN"
expect 200 "alice can edit her own post"

# ---------------------------------------------------------------------------
echo
echo "Comments"
# ---------------------------------------------------------------------------
request POST "/posts/${POST_ID}/comments" '{"content":"First!"}' "$MALLORY_TOKEN"
expect 201 "mallory comments on alice's post"
COMMENT_ID="$(echo "$BODY" | jq -r '.data.id')"

request POST "/posts/${POST_ID}/comments" "{\"content\":\"A reply\",\"parent_id\":\"${COMMENT_ID}\"}" "$ALICE_TOKEN"
expect 201 "alice replies to the comment"

request GET "/posts/${POST_ID}"
expect 200 "the post's comment counter was updated in the same transaction"
COUNT="$(echo "$BODY" | jq -r '.data.comment_count')"
if [ "$COUNT" = "2" ]; then
  printf '  %s✓%s %-58s %s\n' "$GREEN" "$RESET" "comment_count is 2" "$COUNT"
  PASSED=$((PASSED + 1))
else
  printf '  %s✗%s %-58s %s (wanted 2)\n' "$RED" "$RESET" "comment_count is 2" "$COUNT"
  FAILED=$((FAILED + 1))
fi

request GET "/posts/${POST_ID}/comments"
expect 200 "the thread lists both comments"

request PATCH "/comments/${COMMENT_ID}" '{"content":"Edited by the post author"}' "$ALICE_TOKEN"
expect 403 "the post's author cannot edit someone else's comment"

request PATCH "/comments/${COMMENT_ID}" '{"content":"Edited by its author"}' "$MALLORY_TOKEN"
expect 200 "the comment's author can edit it"

request POST "/posts/${DRAFT_ID}/comments" '{"content":"probing"}' "$MALLORY_TOKEN"
expect 404 "commenting on an invisible draft returns 404"

# ---------------------------------------------------------------------------
echo
echo "Sessions"
# ---------------------------------------------------------------------------
request POST /auth/refresh "{\"refresh_token\":\"${ALICE_REFRESH}\"}"
expect 200 "the refresh token can be exchanged once"
NEW_REFRESH="$(echo "$BODY" | jq -r '.data.refresh_token')"

request POST /auth/refresh "{\"refresh_token\":\"${ALICE_REFRESH}\"}"
expect 401 "replaying the rotated refresh token is rejected"

request POST /auth/logout "{\"refresh_token\":\"${NEW_REFRESH}\"}"
expect 204 "logout revokes the session"

request POST /auth/logout "{\"refresh_token\":\"${NEW_REFRESH}\"}"
expect 204 "logout is idempotent"

# ---------------------------------------------------------------------------
echo
echo "Deletion and error handling"
# ---------------------------------------------------------------------------
request DELETE "/posts/${POST_ID}" '' "$ALICE_TOKEN"
expect 204 "alice deletes her own post"

request GET "/posts/${POST_ID}"
expect 404 "the deleted post is gone"

request GET "/comments/${COMMENT_ID}"
expect 404 "its comments went with it"

request DELETE "/posts/${POST_ID}" '' "$ALICE_TOKEN"
expect 404 "deleting twice returns 404"

request GET "/posts/not-a-uuid"
expect 422 "a malformed UUID is a validation error"

request GET /no-such-endpoint
expect 404 "an unknown endpoint uses the error envelope"

# ---------------------------------------------------------------------------
echo
if [ "$FAILED" -eq 0 ]; then
  echo "${GREEN}all ${PASSED} checks passed${RESET}"
  exit 0
fi
echo "${RED}${FAILED} of $((PASSED + FAILED)) checks failed${RESET}"
exit 1
