#!/bin/bash
# manage.sh — operator lifecycle for sqlpod: build, deploy, and secrets.
# Running queries is query.sh's job (the agent-facing script).

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/scripts/lib.sh"

readonly SCRIPT_NAME=$(basename "$0")
readonly APP_NAME="sqlpod"
readonly IMAGE_NAME="sqlpod"

IMAGE_TAG="${IMAGE_TAG:-latest}"
REGISTRY="${REGISTRY:-}"
NAMESPACE="${NAMESPACE:-}"

# Optional imagePullSecret. If set, it is copied from the source namespace during
# setup-namespace and referenced by the deployment. Leave empty for public
# registries (e.g. a public GHCR image).
IMAGE_PULL_SECRET="${IMAGE_PULL_SECRET:-}"

# Connection-string secret the deployment reads from. Override to reuse an
# existing secret (e.g. SQLPOD_SECRET_NAME=my-existing-secret).
SQLPOD_SECRET_NAME="${SQLPOD_SECRET_NAME:-sqlpod-conn-secret}"
SQLPOD_SECRET_KEY="${SQLPOD_SECRET_KEY:-conn-string}"

# Comma-separated named connections (e.g. "orders,warehouse") to wire into the
# deployment's env, in addition to the default connection. Each name maps to
# secret keys conn-string-<name> / conn-string-<name>-write, queried with
# `query.sh query --conn <name> ...`.
SQLPOD_CONNECTIONS="${SQLPOD_CONNECTIONS:-}"

show_help() {
    cat << EOF
sqlpod — ad-hoc SQL query runner for Kubernetes v${VERSION}

Usage: $SCRIPT_NAME [OPTIONS] COMMAND [ARGS]

OPTIONS:
    -n, --namespace NS    Kubernetes namespace (or set NAMESPACE env var)
    -r, --registry URL    Image registry (or set REGISTRY env var)
    -t, --tag TAG         Image tag (default: latest)
    -h, --help            Show this help

LIFECYCLE COMMANDS:
    setup-namespace       Create the namespace (and copy IMAGE_PULL_SECRET if set)
    set-conn [--name N] "<CONN>"        Set a read-only connection string
    set-conn-write [--name N] "<CONN>"  Set a write-capable connection string
    clear-conn-write [--name N]         Remove a write-capable connection string
    build                 Build the Docker image
    push                  Push the image to the registry
    deploy                Deploy (or re-deploy) to the namespace
    delete                Delete the deployment
    status                Show deployment/pod status
    logs [--follow] [--tail N]   Show pod logs

Queries are run with query.sh (the agent-facing script):
    ./query.sh query "SELECT TOP 10 * FROM dbo.Orders"

EXAMPLES:
    export NAMESPACE=dev-alice
    export REGISTRY=ghcr.io/youruser
    $SCRIPT_NAME setup-namespace
    $SCRIPT_NAME set-conn "sqlserver://user:pass@host:1433?database=mydb"
    $SCRIPT_NAME build && $SCRIPT_NAME push && $SCRIPT_NAME deploy

Named connections (one pod, several databases — queried with query.sh --conn):
    $SCRIPT_NAME set-conn --name orders "postgres://reader:pass@pg:5432/orders"
    $SCRIPT_NAME set-conn-write --name orders "postgres://writer:pass@pg:5432/orders"
    $SCRIPT_NAME set-conn --name warehouse "mysql://reader:pass@my:3306/warehouse"
    SQLPOD_CONNECTIONS=orders,warehouse $SCRIPT_NAME deploy

Reuse an existing secret instead of sqlpod-conn-secret:
    SQLPOD_SECRET_NAME=my-existing-secret SQLPOD_SECRET_KEY=conn-string $SCRIPT_NAME deploy
EOF
}

require_registry() {
    if [ -z "$REGISTRY" ]; then
        log_error "Registry not specified"
        log_error "Use -r/--registry flag or set REGISTRY environment variable (e.g. REGISTRY=ghcr.io/youruser)"
        exit 1
    fi
}

full_image_name() { echo "${REGISTRY}/${IMAGE_NAME}:${IMAGE_TAG}"; }

cmd_build() {
    require_command docker
    require_registry
    local img; img=$(full_image_name)
    log_info "Building image: ${img}"
    docker build -t "$img" -f "${SCRIPT_DIR}/Dockerfile" "${SCRIPT_DIR}"
    log_success "Image built: ${img}"
}

cmd_push() {
    require_command docker
    require_registry
    local img; img=$(full_image_name)
    log_info "Pushing image: ${img}"
    docker push "$img"
    log_success "Image pushed: ${img}"
}

cmd_deploy() {
    require_namespace
    require_registry
    check_kubectl
    require_command envsubst

    local img; img=$(full_image_name)
    log_info "Deploying ${APP_NAME} to namespace ${NAMESPACE} (image: ${img})"

    local tmp; tmp=$(mktemp -d)
    cp -r "${SCRIPT_DIR}/k8s/." "$tmp/"

    # imagePullSecrets is included only when IMAGE_PULL_SECRET is set. The
    # substituted block must carry the template's indentation on every line.
    local IMAGE_PULL_SECRETS_BLOCK=""
    if [ -n "$IMAGE_PULL_SECRET" ]; then
        IMAGE_PULL_SECRETS_BLOCK=$'imagePullSecrets:\n      - name: '"${IMAGE_PULL_SECRET}"
    fi

    # Named-connection env entries, generated from SQLPOD_CONNECTIONS. Both
    # keys are optional so the pod starts even before the secret keys exist;
    # queries against a missing key fail with the exact env-var name. The
    # first substituted line continues the template's 8-space indentation;
    # every following line carries its own.
    local NAMED_CONN_ENV_BLOCK=""
    if [ -n "$SQLPOD_CONNECTIONS" ]; then
        local name lc uc
        for name in $(echo "$SQLPOD_CONNECTIONS" | tr ',' ' '); do
            validate_conn_name "$name"
            lc=$(echo "$name" | tr '[:upper:]' '[:lower:]')
            uc=$(echo "$lc" | tr '[:lower:]-' '[:upper:]_')
            [ -n "$NAMED_CONN_ENV_BLOCK" ] && NAMED_CONN_ENV_BLOCK+=$'\n        '
            NAMED_CONN_ENV_BLOCK+="- name: SQLPOD_CONN_${uc}"$'\n          valueFrom:\n            secretKeyRef:\n              name: '"${SQLPOD_SECRET_NAME}"$'\n              key: conn-string-'"${lc}"$'\n              optional: true'
            NAMED_CONN_ENV_BLOCK+=$'\n        '"- name: SQLPOD_CONN_${uc}_WRITE"$'\n          valueFrom:\n            secretKeyRef:\n              name: '"${SQLPOD_SECRET_NAME}"$'\n              key: conn-string-'"${lc}"$'-write\n              optional: true'
        done
        log_info "Named connections wired into deployment: ${SQLPOD_CONNECTIONS}"
    fi

    export SQLPOD_SECRET_NAME SQLPOD_SECRET_KEY IMAGE_PULL_SECRETS_BLOCK NAMED_CONN_ENV_BLOCK
    envsubst '${SQLPOD_SECRET_NAME} ${SQLPOD_SECRET_KEY} ${IMAGE_PULL_SECRETS_BLOCK} ${NAMED_CONN_ENV_BLOCK}' \
        < "$tmp/deployment-template.yaml" > "$tmp/deployment.yaml"
    rm "$tmp/deployment-template.yaml"

    sed -i.bak "s|image: ${IMAGE_NAME}:latest|image: ${img}|g" "$tmp/deployment.yaml"
    rm -f "$tmp/deployment.yaml.bak"

    apply_manifests "$NAMESPACE" "$tmp"
    rm -rf "$tmp"

    log_info "Triggering rolling restart to pull the latest image..."
    kubectl rollout restart deployment "$APP_NAME" -n "$NAMESPACE"
    wait_for_deployment_ready "$NAMESPACE" "$APP_NAME" 120
    log_success "Deployment ready: ${APP_NAME}"
}

cmd_delete() {
    require_namespace
    check_kubectl
    kubectl delete deployment "$APP_NAME" -n "$NAMESPACE" --ignore-not-found=true
    log_success "Deleted ${APP_NAME} from ${NAMESPACE}"
}

cmd_status() {
    require_namespace
    check_kubectl
    kubectl get deployment,pods -n "$NAMESPACE" -l "app=${APP_NAME}"
}

cmd_logs() {
    require_namespace
    check_kubectl
    local follow=false tail=50
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --follow|-f) follow=true; shift ;;
            --tail) tail="$2"; shift 2 ;;
            *) shift ;;
        esac
    done
    local pod; pod=$(get_pod_name "$NAMESPACE" "$APP_NAME") || exit 1
    get_pod_logs "$NAMESPACE" "$pod" "$follow" "$tail"
}

# copy_secret strips namespace-bound metadata and re-applies into the target
# namespace.
copy_secret() {
    local name="$1" src_ns="${2:-default}"
    if ! kubectl get secret "$name" -n "$src_ns" &>/dev/null; then
        log_warn "Secret '${name}' not found in '${src_ns}' — skipping"
        return 1
    fi
    log_info "Copying secret: ${name}"
    kubectl get secret "$name" -n "$src_ns" -o yaml \
        | sed -e '/namespace:/d' -e '/resourceVersion:/d' -e '/uid:/d' -e '/creationTimestamp:/d' \
        | kubectl apply -n "$NAMESPACE" -f -
}

cmd_setup_namespace() {
    require_namespace
    check_kubectl
    if [ "$NAMESPACE" = "default" ]; then
        log_error "Refusing to set up the 'default' namespace — use a developer namespace"
        exit 1
    fi
    if kubectl get namespace "$NAMESPACE" &>/dev/null; then
        log_info "Namespace '${NAMESPACE}' already exists"
    else
        log_info "Creating namespace: ${NAMESPACE}"
        kubectl create namespace "$NAMESPACE"
    fi
    if [ -n "$IMAGE_PULL_SECRET" ]; then
        copy_secret "$IMAGE_PULL_SECRET" || true
    fi
    log_success "Namespace '${NAMESPACE}' is ready"
    log_info "Next: ${SCRIPT_NAME} set-conn \"<connection-string>\""
}

# validate_conn_name enforces the same rule as the pod binary's --conn flag:
# letters, digits, and dashes, starting with a letter. Names are lowercased for
# secret keys; the binary uppercases them (with - → _) for env-var lookup.
validate_conn_name() {
    if ! [[ "$1" =~ ^[A-Za-z][A-Za-z0-9-]*$ ]]; then
        log_error "Invalid connection name '$1' (want letters, digits, and dashes, starting with a letter)"
        exit 1
    fi
}

# parse_conn_cmd_args handles [--name N] plus an optional value for the
# set-conn/set-conn-write/clear-conn-write commands. Sets CONN_NAME (lowercased)
# and CONN_VALUE.
parse_conn_cmd_args() {
    CONN_NAME=""
    CONN_VALUE=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --name)
                [ -z "${2:-}" ] && { log_error "--name requires a value"; exit 1; }
                validate_conn_name "$2"
                CONN_NAME=$(echo "$2" | tr '[:upper:]' '[:lower:]')
                shift 2
                ;;
            *) CONN_VALUE="$1"; shift ;;
        esac
    done
}

# read_key_name/write_key_name map a connection name to its secret keys. The
# default (unnamed) read key stays overridable via SQLPOD_SECRET_KEY.
read_key_name() {
    if [ -n "$CONN_NAME" ]; then echo "conn-string-${CONN_NAME}"; else echo "$SQLPOD_SECRET_KEY"; fi
}
write_key_name() {
    if [ -n "$CONN_NAME" ]; then echo "conn-string-${CONN_NAME}-write"; else echo "conn-string-write"; fi
}

# json_escape emits a JSON string literal (quotes included) for embedding a
# value in a kubectl patch.
json_escape() {
    local s="$1"
    s=${s//\\/\\\\}
    s=${s//\"/\\\"}
    printf '"%s"' "$s"
}

# set_conn_key writes a single key into the connection secret. Uses a merge
# patch, which touches only that key — every other key in the secret is
# preserved by construction (secrets are never `kubectl apply`'d, so apply's
# 3-way-merge key-dropping behavior is out of the picture entirely).
set_conn_key() {
    require_namespace
    check_kubectl
    local key="$1" value="$2"
    if [ -z "$value" ]; then
        log_error "No connection string provided"
        exit 1
    fi

    if kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" &>/dev/null; then
        kubectl patch secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" --type=merge \
            -p "{\"stringData\":{$(json_escape "$key"):$(json_escape "$value")}}" >/dev/null
    else
        kubectl create secret generic "$SQLPOD_SECRET_NAME" \
            --namespace "$NAMESPACE" \
            --from-literal="${key}=${value}" >/dev/null
    fi
    log_success "Set ${SQLPOD_SECRET_NAME}/${key} in ${NAMESPACE}"
    log_info "Restart the pod to pick it up: ${SCRIPT_NAME} deploy"
}

# clear_conn_key removes a single key from the connection secret, leaving all
# other keys untouched (JSON merge patch: null deletes the key).
clear_conn_key() {
    require_namespace
    check_kubectl
    local key="$1"

    if ! kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" &>/dev/null; then
        log_warn "Secret '${SQLPOD_SECRET_NAME}' not found in '${NAMESPACE}' — nothing to clear"
        return 0
    fi

    local val
    val=$(kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" \
            -o "jsonpath={.data['${key}']}" 2>/dev/null)
    if [ -z "$val" ]; then
        log_info "No ${key} set in ${SQLPOD_SECRET_NAME} — nothing to clear"
        return 0
    fi

    kubectl patch secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" --type=merge \
        -p "{\"data\":{$(json_escape "$key"):null}}" >/dev/null
    log_success "Cleared ${key} from ${SQLPOD_SECRET_NAME} in ${NAMESPACE}"
    log_info "Restart the pod to pick it up: ${SCRIPT_NAME} deploy"
}

main() {
    parse_args "$@"
    shift "$ARGS_SHIFT"
    local command="${1:-help}"
    shift || true

    case "$command" in
        build)           cmd_build ;;
        push)            cmd_push ;;
        deploy)          cmd_deploy ;;
        delete)          cmd_delete ;;
        status)          cmd_status ;;
        logs)            cmd_logs "$@" ;;
        setup-namespace) cmd_setup_namespace ;;
        set-conn)
            parse_conn_cmd_args "$@"
            set_conn_key "$(read_key_name)" "$CONN_VALUE"
            ;;
        set-conn-write)
            parse_conn_cmd_args "$@"
            set_conn_key "$(write_key_name)" "$CONN_VALUE"
            ;;
        clear-conn-write)
            parse_conn_cmd_args "$@"
            clear_conn_key "$(write_key_name)"
            ;;
        query|query-file)
            log_error "Queries moved to query.sh: ./query.sh ${command} ..."
            exit 1
            ;;
        help|--help|-h)  show_help ;;
        *) log_error "Unknown command: ${command}"; show_help; exit 1 ;;
    esac
}

main "$@"
