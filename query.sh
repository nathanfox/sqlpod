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
query.sh — run SQL through the sqlpod pod v${VERSION}

Usage: $SCRIPT_NAME [OPTIONS] COMMAND [ARGS]

OPTIONS:
    -n, --namespace NS    Kubernetes namespace (or set NAMESPACE env var)
    -h, --help            Show this help

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

cmd_query_file() {
    local passthru=() file=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            # Flags that take a separate value argument.
            --max-rows|--timeout|--format|--conn) passthru+=("$1" "$2"); shift 2 ;;
            -*) passthru+=("$1"); shift ;;
            *) file="$1"; shift ;;
        esac
    done
    if [ -z "$file" ] || [ ! -f "$file" ]; then
        log_error "query-file requires a path to an existing .sql file"
        exit 1
    fi
    run_query --stdin "${passthru[@]}" < "$file"
}

main() {
    parse_args "$@"
    shift "$ARGS_SHIFT"
    local command="${1:-help}"
    shift || true

    case "$command" in
        query)           run_query "$@" ;;
        query-file)      cmd_query_file "$@" ;;
        help|--help|-h)  show_help ;;
        *) log_error "Unknown command: ${command}"; show_help; exit 1 ;;
    esac
}

main "$@"
