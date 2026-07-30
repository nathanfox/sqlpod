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
    set-conn "<CONN>"     Set the read-only connection string secret
    set-conn-write "<CONN>"  Set the write-capable connection string secret
    clear-conn-write      Remove the write-capable connection string secret
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

    export SQLPOD_SECRET_NAME SQLPOD_SECRET_KEY IMAGE_PULL_SECRETS_BLOCK
    envsubst '${SQLPOD_SECRET_NAME} ${SQLPOD_SECRET_KEY} ${IMAGE_PULL_SECRETS_BLOCK}' \
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

# set_conn_key writes a single key into the connection secret without disturbing
# any other key already present. The manifest handed to `kubectl apply` must
# therefore carry every key we want to keep: apply does a 3-way merge and DROPS
# any key that is missing from the manifest but present in the last-applied
# config. So we read the counterpart key back and re-supply it; otherwise
# set-conn and set-conn-write would clobber each other.
set_conn_key() {
    require_namespace
    check_kubectl
    local key="$1" value="$2"
    if [ -z "$value" ]; then
        log_error "No connection string provided"
        exit 1
    fi

    local -a literals=(--from-literal="${key}=${value}")

    # Preserve the counterpart connection key if it already exists.
    local other
    if [ "$key" = "conn-string-write" ]; then
        other="conn-string"
    else
        other="conn-string-write"
    fi
    local otherval
    otherval=$(kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" \
                 -o "jsonpath={.data['${other}']}" 2>/dev/null | base64 -d 2>/dev/null)
    if [ -n "$otherval" ]; then
        literals+=(--from-literal="${other}=${otherval}")
    fi

    kubectl create secret generic "$SQLPOD_SECRET_NAME" \
        --namespace "$NAMESPACE" \
        "${literals[@]}" \
        --dry-run=client -o yaml | kubectl apply -f -
    log_success "Set ${SQLPOD_SECRET_NAME}/${key} in ${NAMESPACE}"
    log_info "Restart the pod to pick it up: ${SCRIPT_NAME} deploy"
}

# clear_conn_write drops the write-capable key from the connection secret while
# preserving the read-only key. Same merge caveat as set_conn_key: the manifest
# we apply carries only the read key, so `apply`'s 3-way merge DROPS
# conn-string-write from the live secret.
clear_conn_write() {
    require_namespace
    check_kubectl

    if ! kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" &>/dev/null; then
        log_warn "Secret '${SQLPOD_SECRET_NAME}' not found in '${NAMESPACE}' — nothing to clear"
        return 0
    fi

    local writeval
    writeval=$(kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" \
                 -o "jsonpath={.data['conn-string-write']}" 2>/dev/null | base64 -d 2>/dev/null)
    if [ -z "$writeval" ]; then
        log_info "No write connection string set in ${SQLPOD_SECRET_NAME} — nothing to clear"
        return 0
    fi

    # Preserve the read-only key if it exists; otherwise nothing is left to keep.
    local readval
    readval=$(kubectl get secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE" \
                -o "jsonpath={.data['${SQLPOD_SECRET_KEY}']}" 2>/dev/null | base64 -d 2>/dev/null)

    if [ -n "$readval" ]; then
        kubectl create secret generic "$SQLPOD_SECRET_NAME" \
            --namespace "$NAMESPACE" \
            --from-literal="${SQLPOD_SECRET_KEY}=${readval}" \
            --dry-run=client -o yaml | kubectl apply -f -
    else
        kubectl delete secret "$SQLPOD_SECRET_NAME" -n "$NAMESPACE"
    fi
    log_success "Cleared write connection string from ${SQLPOD_SECRET_NAME} in ${NAMESPACE}"
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
        set-conn)        set_conn_key "$SQLPOD_SECRET_KEY" "$1" ;;
        set-conn-write)  set_conn_key "conn-string-write" "$1" ;;
        clear-conn-write) clear_conn_write ;;
        query|query-file)
            log_error "Queries moved to query.sh: ./query.sh ${command} ..."
            exit 1
            ;;
        help|--help|-h)  show_help ;;
        *) log_error "Unknown command: ${command}"; show_help; exit 1 ;;
    esac
}

main "$@"
