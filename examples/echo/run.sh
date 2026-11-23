#!/bin/sh
set -eu

if [ "$#" -eq 0 ]; then
    packaged=false
elif [ "$#" -eq 1 ] && [ "$1" = --packaged ]; then
    packaged=true
else
    printf '%s\n' 'usage: sh examples/echo/run.sh [--packaged]' >&2
    exit 2
fi

: "${KUBE_CONTEXT:?set the development Kubernetes context}"
: "${ATE_CA:?set the path to the ATE Control CA bundle}"
: "${ATE_TOKEN_FILE:?set the path to the Echo deployer token}"
: "${ROUTER_CA:?set the path to the ATE Router CA bundle}"
: "${ROUTER_CERT:?set the path to the Router client certificate}"
: "${ROUTER_KEY:?set the path to the Router client private key}"

example_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$example_dir/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/worker-platform-echo.XXXXXXXX")
pids=
cleanup() {
    for pid in $pids; do
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
    done
    rm -f -- "$work/build.json" "$work/worker.yaml" "$work/profile.yaml" "$work/echo-app" \
        "$work/control.log" "$work/router.log" "$work/gateway.log" "$work/response.json" \
        "$work/package/oci-layout" "$work/package/index.json" "$work/package/blobs/sha256/"*
    rmdir -- "$work/package/blobs/sha256" "$work/package/blobs" "$work/package" "$work" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

for path in "$ATE_CA" "$ATE_TOKEN_FILE" "$ROUTER_CA" "$ROUTER_CERT" "$ROUTER_KEY"; do
    [ -f "$path" ] && [ -r "$path" ] || fail "cannot read credential file: $path"
done

kube() {
    kubectl --context "$KUBE_CONTEXT" "$@"
}

start_forward() {
    namespace=$1
    service=$2
    port=$3
    log=$4
    kubectl --context "$KUBE_CONTEXT" -n "$namespace" port-forward --address 127.0.0.1 \
        "svc/$service" ":$port" >"$log" 2>&1 &
    forward_pid=$!
    pids="$pids $forward_pid"
    remaining=30
    while [ "$remaining" -gt 0 ]; do
        forwarded_address=$(sed -n 's/^Forwarding from \(127\.0\.0\.1:[0-9][0-9]*\) -> .*/\1/p' "$log")
        [ -z "$forwarded_address" ] || return 0
        kill -0 "$forward_pid" 2>/dev/null || break
        sleep 1
        remaining=$((remaining - 1))
    done
    cat "$log" >&2
    fail "could not port-forward $namespace/$service"
}

# Validate the example's dedicated dependencies before publishing artifacts.
kube -n wp-echo-example get gateway echo --request-timeout=15s >/dev/null
kube -n ate-system get workerpool echo-arm64-dev --request-timeout=15s >/dev/null
kube -n ate-system get svc wp-echo-example-router --request-timeout=15s >/dev/null

publish_image() {
    docker buildx build --platform linux/arm64 --file "$repo_root/$1" \
        --tag "$2:$3" --push --metadata-file "$work/build.json" "$repo_root" >&2 || return 1
    jq -er '."containerimage.digest"' "$work/build.json"
}

require_digest_ref() {
    pattern='^[a-z0-9][a-z0-9./_-]*(:[0-9]+)?/[a-z0-9][a-z0-9./_-]*@sha256:[0-9a-f]{64}$'
    valid_ref=$(printf '%s\n' "$1" | grep -E "$pattern") || fail "$2 must be pinned by SHA-256"
    [ "$valid_ref" = "$1" ] || fail "expected one $2 reference"
}

payload='{"message":"echo-example"}'
expected=$payload
profile_file="$example_dir/ate-profile.yaml"
if [ "$packaged" = true ]; then
    : "${RUNTIME_REPOSITORY:?set the common runtime image repository}"
    : "${PACKAGE_REPOSITORY:?set the Echo app package repository}"
    revision=$(git -C "$repo_root" rev-parse --short=12 HEAD)
    version="ate-smoke-$revision-${work##*.}"
    runtime_digest=$(publish_image Dockerfile.actor-runtime "${RUNTIME_PUSH_REPOSITORY:-$RUNTIME_REPOSITORY}" "$version-runtime")
    runtime_image="$RUNTIME_REPOSITORY@$runtime_digest"
    require_digest_ref "$runtime_image" "runtime image"
    (
        cd "$repo_root"
        CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=$version" \
            -o "$work/echo-app" ./examples/echo/app
    )
    package_digest=$(python3 "$repo_root/scripts/build-app-package.py" "$work/echo-app" "$work/package")
    app_package="$PACKAGE_REPOSITORY@$package_digest"
    require_digest_ref "$app_package" "app package"
    package_tag="${PACKAGE_PUSH_REPOSITORY:-$PACKAGE_REPOSITORY}:$version"
    crane push "$work/package" "$package_tag" >&2
    published_digest=$(crane digest "$package_tag")
    [ "$published_digest" = "$package_digest" ] ||
        fail "published Echo package does not match its OCI manifest digest"
    python3 "$example_dir/packaged_smoke.py" "$work" "$runtime_image" "$app_package"
    profile_file="$work/profile.yaml"
    expected=$(printf '{"message":"echo-example","version":"%s"}' "$version")
    printf 'Runtime image %s and Echo package %s\n' "$runtime_digest" "$package_digest" >&2
else
    image=${ECHO_IMAGE:-}
    if [ -z "$image" ]; then
        : "${ECHO_REPOSITORY:?set an image repository or provide ECHO_IMAGE}"
        tag=${ECHO_TAG:-echo-$(date -u +%Y%m%d%H%M%S)}
        digest=$(publish_image Dockerfile.echo "$ECHO_REPOSITORY" "$tag")
        image="$ECHO_REPOSITORY@$digest"
    fi
    require_digest_ref "$image" "Echo image"
    sed "s|^  image: .*|  image: $image|" "$example_dir/ate-worker.yaml" >"$work/worker.yaml"
fi

start_forward ate-system api 443 "$work/control.log"
ate_address=$forwarded_address
start_forward ate-system wp-echo-example-router 443 "$work/router.log"
router_address=$forwarded_address

# The Go command runs in the module directory. Resolve credential files first
# so callers can use paths relative to their current directory.
absolute_path() {
    dir=$(CDPATH= cd -- "$(dirname -- "$1")" && pwd)
    printf '%s/%s\n' "$dir" "$(basename -- "$1")"
}
ate_ca=$(absolute_path "$ATE_CA")
ate_token=$(absolute_path "$ATE_TOKEN_FILE")
router_ca=$(absolute_path "$ROUTER_CA")
router_cert=$(absolute_path "$ROUTER_CERT")
router_key=$(absolute_path "$ROUTER_KEY")
(
    cd "$repo_root"
    go run ./cmd/workerctl deploy -f "$work/worker.yaml" -profile "$profile_file" \
        -kube-context "$KUBE_CONTEXT" \
        -ate-address "$ate_address" -ate-server-name "${ATE_SERVER_NAME:-api.ate-system.svc}" \
        -ate-ca "$ate_ca" -ate-token-file "$ate_token" \
        -router-url "https://$router_address" -router-server-name "${ROUTER_SERVER_NAME:-atenet-router.ate-system.svc}" \
        -router-ca "$router_ca" -router-cert "$router_cert" -router-key "$router_key"
)

start_forward wp-echo-example echo 80 "$work/gateway.log"
url="http://$forwarded_address/invoke"
call_echo() {
    curl --noproxy '*' -fsS --max-time 5 -w '%{http_code}' -o "$work/response.json" -H 'Host: echo.example.test' \
        -H 'Content-Type: application/json' --data "$payload" "$@" "$url"
}
remaining=15
while [ "$remaining" -gt 0 ]; do
    if status=$(call_echo 2>/dev/null) && [ "$status" = 200 ] && [ "$(cat "$work/response.json")" = "$expected" ]; then
        break
    fi
    sleep 1
    remaining=$((remaining - 1))
done
if [ "$remaining" -eq 0 ]; then
    call_echo >&2 || true
    [ ! -f "$work/response.json" ] || cat "$work/response.json" >&2
    fail "Gateway did not return the expected Echo response"
fi
status=$(call_echo -H 'ate-target-actor: wp-echo-example/unassigned-test-name')
[ "$status" = 200 ] && [ "$(cat "$work/response.json")" = "$expected" ] ||
    fail "Gateway did not overwrite the target header"
# The Gateway publishes the invocation endpoint and keeps the readiness probe private.
status=$(curl --noproxy '*' -sS --max-time 5 -w '%{http_code}' -o /dev/null \
    -H 'Host: echo.example.test' "http://$forwarded_address/readyz")
[ "$status" = 404 ] || fail "Gateway exposed the private readiness probe"
printf '%s\n' "$expected"
