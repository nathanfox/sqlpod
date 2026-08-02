#!/bin/bash
# integration-test.sh — end-to-end test of manage.sh and query.sh against the
# cluster behind the current kubectl context.
#
# Requires: docker, kubectl (context on a docker-runtime cluster, e.g. colima
# k3s or minikube with --driver=docker), envsubst, jq. Everything runs in a
# throwaway namespace with an in-cluster Postgres; images go through a local
# registry container so `imagePullPolicy: Always` works unmodified.
#
# Usage: ./scripts/integration-test.sh
#   KEEP=1   keep the namespace (and registry) around for debugging

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(dirname "$SCRIPT_DIR")"

readonly REGISTRY_NAME="sqlpod-itest-registry"
readonly REGISTRY_PORT=5001   # 5000 collides with macOS AirPlay
readonly PG_PASSWORD="itest-pw"

export NAMESPACE="sqlpod-itest-$$"
export REGISTRY="localhost:${REGISTRY_PORT}"

readonly COLOR_RED='\033[0;31m'
readonly COLOR_GREEN='\033[0;32m'
readonly COLOR_BLUE='\033[0;34m'
readonly COLOR_RESET='\033[0m'

log_info()    { echo -e "${COLOR_BLUE}[INFO]${COLOR_RESET} $*"; }
log_error()   { echo -e "${COLOR_RED}[ERROR]${COLOR_RESET} $*" >&2; }

PASS_COUNT=0
FAIL_COUNT=0

check_pass() { echo -e "${COLOR_GREEN}[PASS]${COLOR_RESET} $*"; PASS_COUNT=$((PASS_COUNT + 1)); }
check_fail() { echo -e "${COLOR_RED}[FAIL]${COLOR_RESET} $*" >&2; FAIL_COUNT=$((FAIL_COUNT + 1)); }

# assert_jq JSON JQ_EXPR DESCRIPTION — passes when the jq expression is truthy.
assert_jq() {
    local json=$1 expr=$2 desc=$3
    if jq -e "$expr" <<< "$json" > /dev/null 2>&1; then
        check_pass "$desc"
    else
        check_fail "$desc — jq '$expr' not satisfied by: $json"
    fi
}

# assert_query DESCRIPTION JQ_EXPR QUERY_SH_ARGS... — runs query.sh and checks
# the JSON result; a failing query is a FAIL, not a script abort.
assert_query() {
    local desc=$1 expr=$2 out
    shift 2
    if out=$("$ROOT/query.sh" "$@"); then
        assert_jq "$out" "$expr" "$desc"
    else
        check_fail "$desc — query.sh $* failed"
    fi
}

STARTED_REGISTRY=false
TMP_DIR=""

cleanup() {
    local rc=$?
    if [ "${KEEP:-0}" = "1" ]; then
        log_info "KEEP=1: leaving namespace $NAMESPACE and registry running"
    else
        kubectl delete namespace "$NAMESPACE" --wait=false > /dev/null 2>&1 || true
        if [ "$STARTED_REGISTRY" = true ]; then
            docker rm -f "$REGISTRY_NAME" > /dev/null 2>&1 || true
        fi
    fi
    if [ -n "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
    exit "$rc"
}
trap cleanup EXIT

# --- Preflight ---------------------------------------------------------------

for cmd in docker kubectl envsubst jq; do
    if ! command -v "$cmd" &> /dev/null; then
        log_error "Required command not found: $cmd"
        exit 1
    fi
done

log_info "kubectl context: $(kubectl config current-context)"
if ! kubectl version --request-timeout=5s > /dev/null 2>&1; then
    log_error "Cluster not reachable via current kubectl context"
    exit 1
fi

runtime=$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.containerRuntimeVersion}')
if [[ "$runtime" != docker://* ]]; then
    log_error "Node runtime is $runtime, not docker:// — pods cannot pull from"
    log_error "the localhost registry without insecure-registry configuration."
    exit 1
fi

TMP_DIR=$(mktemp -d)

# --- Local registry ----------------------------------------------------------

if [ "$(docker inspect -f '{{.State.Running}}' "$REGISTRY_NAME" 2>/dev/null)" = "true" ]; then
    log_info "Reusing running registry container $REGISTRY_NAME"
else
    docker rm -f "$REGISTRY_NAME" > /dev/null 2>&1 || true
    log_info "Starting local registry $REGISTRY_NAME on port $REGISTRY_PORT"
    docker run -d --name "$REGISTRY_NAME" -p "${REGISTRY_PORT}:5000" registry:2 > /dev/null
    STARTED_REGISTRY=true
fi

# --- Namespace, Postgres, secrets --------------------------------------------

log_info "Test namespace: $NAMESPACE"
"$ROOT/manage.sh" setup-namespace

kubectl apply -n "$NAMESPACE" -f - > /dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: postgres:17-alpine
        env:
        - name: POSTGRES_PASSWORD
          value: "$PG_PASSWORD"
        ports:
        - containerPort: 5432
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
EOF

DSN="postgres://postgres:${PG_PASSWORD}@postgres:5432/postgres?sslmode=disable"
"$ROOT/manage.sh" set-conn "$DSN"
"$ROOT/manage.sh" set-conn-write "$DSN"
"$ROOT/manage.sh" set-conn --name orders "$DSN"

# --- Build, push, deploy ------------------------------------------------------

"$ROOT/manage.sh" build
"$ROOT/manage.sh" push
SQLPOD_CONNECTIONS=orders "$ROOT/manage.sh" deploy

if "$ROOT/manage.sh" status > /dev/null; then
    check_pass "manage.sh status exits 0"
else
    check_fail "manage.sh status exits 0"
fi

kubectl rollout status deployment/postgres -n "$NAMESPACE" --timeout=90s > /dev/null

# --- Query assertions ---------------------------------------------------------

# Postgres may still be initializing right after rollout; retry the first query.
first=""
last_err=""
for _ in $(seq 1 15); do
    if first=$("$ROOT/query.sh" query "SELECT 1 AS one" 2> "$TMP_DIR/query-err"); then
        break
    fi
    first=""
    last_err=$(cat "$TMP_DIR/query-err")
    sleep 2
done
if [ -z "$first" ]; then
    check_fail "basic read query returned a result"
    log_error "Postgres never became queryable; aborting remaining checks"
    log_error "last query error: ${last_err}"
    exit 1
fi
assert_jq "$first" '.mode == "read"' "read query reports mode=read"
assert_jq "$first" '.columns == ["one"]' "read query returns columns"
assert_jq "$first" '.rows[0][0] == 1 and .rowCount == 1' "read query returns rows"

echo "SELECT 2 AS two" > "$TMP_DIR/q.sql"
assert_query "query-file runs SQL from a file" '.rows[0][0] == 2' \
    query-file "$TMP_DIR/q.sql"

if out=$("$ROOT/query.sh" query --format tsv "SELECT 3 AS three") \
    && [ "$out" = "$(printf 'three\n3')" ]; then
    check_pass "tsv format prints header and row"
else
    check_fail "tsv format prints header and row — got: $out"
fi

assert_query "--max-rows truncates" '.truncated == true and .rowCount == 2' \
    query --max-rows 2 "SELECT * FROM generate_series(1,5)"

set +e
err=$("$ROOT/query.sh" query "CREATE TABLE itest (id int)" 2>&1 > /dev/null)
rc=$?
set -e
if [ "$rc" -ne 0 ] && grep -q '"error"' <<< "$err"; then
    check_pass "DDL without --write is rejected"
else
    check_fail "DDL without --write is rejected — rc=$rc, stderr: $err"
fi
if grep -q "$PG_PASSWORD" <<< "$err"; then
    check_fail "error output does not leak the connection password"
else
    check_pass "error output does not leak the connection password"
fi

assert_query "--write runs DDL" '.mode == "write"' \
    query --write "CREATE TABLE itest (id int)"
assert_query "--write reports rowsAffected" '.rowsAffected == 2' \
    query --write "INSERT INTO itest VALUES (1),(2)"
assert_query "written rows are readable back" '.rows[0][0] == 2' \
    query "SELECT count(*) AS n FROM itest"

assert_query "named connection (--conn orders) works" '.rows[0][0] == 4' \
    query --conn orders "SELECT 4 AS four"

if "$ROOT/manage.sh" logs > /dev/null; then
    check_pass "manage.sh logs exits 0"
else
    check_fail "manage.sh logs exits 0"
fi

# --- Summary ------------------------------------------------------------------

echo
if [ "$FAIL_COUNT" -eq 0 ]; then
    check_pass "all $PASS_COUNT checks passed"
else
    log_error "$FAIL_COUNT of $((PASS_COUNT + FAIL_COUNT)) checks failed"
    exit 1
fi
