#!/bin/bash
# query.sh — run SQL through the sqlpod pod.
#
# This is the agent-facing script: it can only run queries. Deployment and
# secret management live in manage.sh, so granting a tool (e.g. an AI agent)
# permission to run query.sh never grants it deploy/delete/credential access.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/scripts/lib.sh"

readonly SCRIPT_NAME=$(basename "$0")
readonly APP_NAME="sqlpod"

NAMESPACE="${NAMESPACE:-}"

show_help() {
    cat << EOF
query.sh — run SQL through the sqlpod pod (${VERSION})

Usage: $SCRIPT_NAME [OPTIONS] COMMAND [ARGS]

OPTIONS:
    -n, --namespace NS    Kubernetes namespace (or set NAMESPACE env var)
    -h, --help            Show this help
    -v, --version         Print the script version

COMMANDS:
    query [FLAGS] "<SQL>"   Run a query, print JSON
    query-file [FLAGS] <FILE>   Run SQL from a file (piped via stdin)

FLAGS (forwarded to the pod binary):
    --write               Use the write connection and COMMIT (default: read-only)
    --conn NAME           Use a named connection (SQLPOD_CONN_<NAME>) instead of the default
    --max-rows N          Maximum rows before truncating (default: 1000)
    --timeout DUR         Overall timeout (default: 30s)
    --format json|tsv     Output format (default: json)

EXAMPLES:
    export NAMESPACE=dev-alice
    $SCRIPT_NAME query "SELECT TOP 10 * FROM dbo.Orders"
    $SCRIPT_NAME query --max-rows 5000 "SELECT * FROM dbo.BigTable"
    $SCRIPT_NAME query-file report.sql
    $SCRIPT_NAME query --write "UPDATE dbo.Orders SET status='x' WHERE id=1"
    $SCRIPT_NAME query --conn warehouse "SELECT COUNT(*) FROM inventory"

Deployment and secrets are managed with manage.sh.
EOF
}

# parse_query_flags ARGS... — validates the agent-facing flag surface and
# splits it from the single positional argument (SQL text or file path).
# Only the documented flags are forwarded: the pod binary accepts more (e.g.
# --file, which reads arbitrary in-pod paths), and none of that may be
# reachable through query.sh. Sets PASSTHRU and POSITIONAL.
parse_query_flags() {
    PASSTHRU=()
    POSITIONAL=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --write)
                PASSTHRU+=("$1"); shift ;;
            --conn|--max-rows|--timeout|--format)
                if [ $# -lt 2 ]; then
                    log_error "$1 requires a value"
                    exit 1
                fi
                PASSTHRU+=("$1" "$2"); shift 2 ;;
            -*)
                log_error "Unknown flag: $1 (allowed: --write, --conn NAME, --max-rows N, --timeout DUR, --format json|tsv)"
                exit 1 ;;
            *)
                if [ -n "$POSITIONAL" ]; then
                    log_error "Unexpected extra argument: $1 (quote the SQL as a single argument)"
                    exit 1
                fi
                POSITIONAL="$1"; shift ;;
        esac
    done
}

cmd_query() {
    parse_query_flags "$@"
    if [ -z "$POSITIONAL" ]; then
        log_error "query requires a SQL string argument"
        exit 1
    fi
    run_query "${PASSTHRU[@]}" "$POSITIONAL"
}

cmd_query_file() {
    parse_query_flags "$@"
    if [ -z "$POSITIONAL" ] || [ ! -f "$POSITIONAL" ]; then
        log_error "query-file requires a path to an existing .sql file"
        exit 1
    fi
    run_query --stdin "${PASSTHRU[@]}" < "$POSITIONAL"
}

main() {
    parse_args "$@"
    shift "$ARGS_SHIFT"
    local command="${1:-help}"
    shift || true

    case "$command" in
        query)           cmd_query "$@" ;;
        query-file)      cmd_query_file "$@" ;;
        help|--help|-h)  show_help ;;
        *) log_error "Unknown command: ${command}"; show_help; exit 1 ;;
    esac
}

main "$@"
