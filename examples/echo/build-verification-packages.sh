#!/bin/sh
set -eu

layout_root=
if [ "$#" -eq 2 ] && [ "$1" = "--layout-only" ]; then
    mkdir -p -- "$2"
    layout_root=$(CDPATH= cd -- "$2" && pwd)
elif [ "$#" -eq 0 ]; then
    : "${PACKAGE_REPOSITORY:?개발 Actor가 읽는 검증 패키지 레지스트리를 지정하세요}"
    : "${PACKAGE_TAG_PREFIX:?검증 실행에 고유한 태그 접두어를 지정하세요}"
else
    printf '%s\n' '사용법: build-verification-packages.sh [--layout-only 출력-폴더]' >&2
    exit 2
fi

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/worker-json-package.XXXXXXXX")
trap 'rm -rf -- "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

build_package() (
    stage=$1
    delay=$2
    candidate="$work/$stage"
    mkdir "$candidate"
    (
        cd "$root"
        CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath \
            -ldflags "-s -w -X main.verificationDelay=$delay" \
            -o "$candidate/echo" ./examples/echo/json-app
    )
    layout="$candidate/layout"
    if [ -n "$layout_root" ]; then
        layout="$layout_root/$stage"
        mkdir -- "$layout"
    fi
    digest=$(python3 "$root/scripts/build-app-package.py" "$candidate/echo" "$layout")
    if [ -n "$layout_root" ]; then
        printf '%s=%s\n' "$stage" "$digest"
        exit 0
    fi

    tag="${PACKAGE_PUSH_REPOSITORY:-$PACKAGE_REPOSITORY}:$PACKAGE_TAG_PREFIX-$stage"
    crane push "$layout" "$tag" >&2
    published=$(crane digest "$tag")
    if [ "$published" != "$digest" ]; then
        printf '%s\n' "게시된 검증 패키지 digest 불일치: $stage" >&2
        exit 1
    fi
    printf '%s=%s@%s\n' "$stage" "$PACKAGE_REPOSITORY" "$digest"
)

build_package package_v1 0s
build_package package_failure -1s
build_package package_v2 "${ECHO_VERIFICATION_DELAY:-8s}"
