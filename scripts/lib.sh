#!/bin/bash
# lib.sh — logging, argument parsing, and kubectl helpers shared by manage.sh
# and query.sh. Self-contained.

readonly VERSION="0.3.0"

readonly COLOR_RED='\033[0;31m'
readonly COLOR_GREEN='\033[0;32m'
readonly COLOR_YELLOW='\033[1;33m'
readonly COLOR_BLUE='\033[0;34m'
readonly COLOR_RESET='\033[0m'

log_info() {
    echo -e "${COLOR_BLUE}[INFO]${COLOR_RESET} $*"
}

log_success() {
    echo -e "${COLOR_GREEN}[SUCCESS]${COLOR_RESET} $*"
}

log_warn() {
    echo -e "${COLOR_YELLOW}[WARN]${COLOR_RESET} $*"
}

log_error() {
    echo -e "${COLOR_RED}[ERROR]${COLOR_RESET} $*" >&2
}

require_command() {
    local cmd=$1
    if ! command -v "$cmd" &> /dev/null; then
        log_error "Required command not found: $cmd"
        log_error "Please install $cmd and try again"
        exit 1
    fi
}

require_namespace() {
    if [ -z "$NAMESPACE" ]; then
        log_error "Namespace not specified"
        log_error "Use -n/--namespace flag or set NAMESPACE environment variable"
        exit 1
    fi
}

# parse_args consumes global OPTIONS (-n/-r/-t/-h/-v) and sets ARGS_SHIFT so the
# caller can `shift` past them to reach the subcommand.
parse_args() {
    ARGS_SHIFT=0
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -n|--namespace)
                NAMESPACE="$2"
                shift 2
                ARGS_SHIFT=$((ARGS_SHIFT + 2))
                ;;
            -r|--registry)
                REGISTRY="$2"
                shift 2
                ARGS_SHIFT=$((ARGS_SHIFT + 2))
                ;;
            -t|--tag)
                IMAGE_TAG="$2"
                shift 2
                ARGS_SHIFT=$((ARGS_SHIFT + 2))
                ;;
            -h|--help)
                show_help
                exit 0
                ;;
            -v|--version)
                echo "${SCRIPT_NAME:-manage.sh} version ${VERSION:-dev}"
                exit 0
                ;;
            -*)
                if [ $# -eq 1 ]; then
                    log_error "Unknown option: $1"
                    show_help
                    exit 1
                fi
                break
                ;;
            *)
                break
                ;;
        esac
    done
}

check_kubectl() {
    require_command kubectl
}

get_pod_name() {
    local namespace=$1
    local app_label=$2

    check_kubectl

    local pod_name
    pod_name=$(kubectl get pods -n "$namespace" -l "app=${app_label}" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -z "$pod_name" ]; then
        log_error "No pod found with label app=${app_label} in namespace $namespace"
        return 1
    fi

    echo "$pod_name"
}

get_pod_logs() {
    local namespace=$1
    local pod_name=$2
    local follow=${3:-false}
    local tail=${4:-50}

    check_kubectl

    local args=("-n" "$namespace")

    if [ "$follow" = "true" ]; then
        args+=("-f")
    fi

    args+=("--tail=$tail" "$pod_name")

    kubectl logs "${args[@]}"
}

wait_for_deployment_ready() {
    local namespace=$1
    local deployment=$2
    local timeout=${3:-120}

    check_kubectl

    log_info "Waiting for deployment to be ready: $deployment (timeout: ${timeout}s)"

    if kubectl wait --for=condition=available deployment/"$deployment" -n "$namespace" --timeout="${timeout}s" 2>/dev/null; then
        log_success "Deployment is ready: $deployment"
        return 0
    else
        log_error "Deployment failed to become ready: $deployment"
        return 1
    fi
}

apply_manifests() {
    local namespace=$1
    local manifest_dir=$2

    check_kubectl

    if [ ! -d "$manifest_dir" ]; then
        log_error "Manifest directory not found: $manifest_dir"
        return 1
    fi

    log_info "Applying manifests from: $manifest_dir"

    kubectl apply -n "$namespace" -f "$manifest_dir"

    log_success "Manifests applied"
}

# run_query execs the binary in the running pod. Uses `-i` (no TTY) so JSON
# stdout comes back clean; SQL never appears in the pod's process args of any
# other container. Expects NAMESPACE and APP_NAME to be set by the caller.
run_query() {
    require_namespace
    check_kubectl
    local stdin=false args=()
    if [ "$1" = "--stdin" ]; then stdin=true; shift; fi
    args=("$@")
    local pod; pod=$(get_pod_name "$NAMESPACE" "$APP_NAME") || exit 1
    if [ "$stdin" = true ]; then
        kubectl exec -i -n "$NAMESPACE" "$pod" -- /sqlpod "${args[@]}"
    else
        kubectl exec -n "$NAMESPACE" "$pod" -- /sqlpod "${args[@]}"
    fi
}
