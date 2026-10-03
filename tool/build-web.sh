#!/usr/bin/env bash
# 本地编译网页后端。
#
# 为什么需要这个脚本：后端用 go:embed 内置界面，而界面按 fnOS 规范必须位于
# app/ui，与源码目录 app/web 分开，直接 `go build ./app/web` 会报
# "pattern ui: no matching files found"。这里把界面复制到临时目录再编译。
#
# 用法:
#   ./tool/build-web.sh              # 本机架构
#   FNOS_GOARCH=arm64 ./tool/build-web.sh
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
ARCH="${FNOS_GOARCH:-amd64}"
OUT="${OUT:-$ROOT/dist-local}"

mkdir -p "$OUT"
SRC=$(mktemp -d)
trap 'rm -rf "$SRC"' EXIT

cp app/web/main.go app/web/main_test.go "$SRC/"
cp -r app/ui/. "$SRC/ui/"
printf 'module cfddns-web\n\ngo 1.24\n' >"$SRC/go.mod"

echo "==> go vet + test"
(cd "$SRC" && go vet ./... && go test ./...)

echo "==> 编译 cfddns-web (linux/${ARCH})"
(cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags "-s -w" -o "$OUT/cfddns-web-linux-${ARCH}" .)

ls -l "$OUT" | sed 's/^/  /'
echo "  版本: $("$OUT/cfddns-web-linux-${ARCH}" --version 2>/dev/null || echo '(跨架构，无法在本机执行)')"
