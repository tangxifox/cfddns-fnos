#!/usr/bin/env bash
# 构建 fnOS 应用包（.fpk）。
#
# 用法:
#   ./build-fpk.sh                    # x86_64
#   FNOS_GOARCH=arm64 ./build-fpk.sh  # ARM64
#
# 说明:
#   fnOS 的正式 .fpk 必须由官方 fnpack 生成，本脚本不自行拼装该格式。
#   缺少 fnpack 时不会伪造 .fpk，而是产出 dist/*-dev.zip 这一「开发包」：
#   结构与安装后的目录一致，便于检视，但它**不能**通过应用中心安装。
#   这样避免给出一个看起来像成品、实际装不上的文件。
#
#   fnpack 下载: https://developer.fnnas.com/docs/cli/fnpack
set -euo pipefail

cd "$(dirname "$0")"
ROOT=$(pwd)
ARCH="${FNOS_GOARCH:-amd64}"
OUT="${OUT:-$ROOT/dist}"
APPNAME=$(sed -n 's/^appname=//p' manifest | head -n 1)
VERSION=$(sed -n 's/^version=//p' manifest | head -n 1)
# 上游 cfddns 的发布版本：本应用封装的就是该 tag 下的官方二进制
UPSTREAM_VERSION="${UPSTREAM_VERSION:-1.0.0}"

# 本机无法执行的架构不在这里跑：架构自检要用 file 读二进制头，
# 与能否执行无关，因此无需 qemu。
case "$ARCH" in
  amd64) WANT_ELF="x86-64" ;;
  arm64) WANT_ELF="aarch64" ;;
  *) WANT_ELF="" ;;
esac

# verify_arch <文件> <说明>
#
# 为什么必须校验：构建脚本的架构来自环境变量，而调用方（CI 矩阵、
# 本地构建）很容易漏传，一旦漏传就会用默认的 amd64 产出「arm64 包」——
# 包名、manifest 的 platform 全写 arm，里面的二进制却是 x86-64。
# 这种包只有装到真机上才会暴露，且很难第一眼看出，因此在这里拦住。
verify_arch() {
  local f="$1" what="$2" got
  [ -n "$WANT_ELF" ] || return 0
  [ -f "$f" ] || { echo "    ✗ ${what} 不存在：$f" >&2; return 1; }
  got=$(file -b "$f")
  case "$got" in
    *"$WANT_ELF"*) printf '    ✓ %s 架构为 %s\n' "$what" "$WANT_ELF" ;;
    *)
      printf '    ✗ %s 架构不符：期望含 %s，实际 %s\n' "$what" "$WANT_ELF" "${got%%:*}" >&2
      return 1
      ;;
  esac
}

mkdir -p "$OUT"
echo "==> 应用: ${APPNAME} ${VERSION}  架构: ${ARCH}  上游: cfddns v${UPSTREAM_VERSION}"

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

# ---------- 1. 图标 ----------
echo "==> 生成图标"
python3 tool/gen-icons.py

# ---------- 2. 取官方二进制 ----------
# 本应用只封装官方仓库发布的二进制，不自行编译：
#   https://github.com/tangxifox/cfddns
# 下载后必须通过官方 SHA256SUMS.txt 校验，避免拿到损坏或被替换的文件；
# 已存在且校验通过时复用缓存，不重复下载。
echo "==> 获取官方 cfddns 二进制 (linux/${ARCH})"
CACHE="${CFDDNS_CACHE:-$HOME/.cache/cfddns-fnos}"
mkdir -p "$CACHE"
BIN_NAME="cfddns-linux-${ARCH}"
BIN_SRC="$CACHE/$BIN_NAME"
SUMS="$CACHE/SHA256SUMS.txt"
RAW_BASE="${CFDDNS_RELEASE_URL:-https://github.com/tangxifox/cfddns/releases/download/v${UPSTREAM_VERSION}}"

fetch() {
  local name="$1" dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --max-time 180 "$RAW_BASE/$name" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -q -T 180 "$RAW_BASE/$name" -O "$dest"
  else
    return 1
  fi
}

verify() {
  local file="$1" name="$2"
  [ -s "$file" ] || return 1
  [ -s "$SUMS" ] || return 1
  local want got
  want=$(awk -v n="$name" '$2 == n { print $1 }' "$SUMS" | head -n 1)
  [ -n "$want" ] || return 1
  got=$(sha256sum "$file" | cut -d' ' -f1)
  [ "$want" = "$got" ]
}

if ! verify "$BIN_SRC" "$BIN_NAME"; then
  echo "    从 ${RAW_BASE} 下载"
  fetch "SHA256SUMS.txt" "$SUMS" || fail "下载 SHA256SUMS.txt 失败"
  fetch "$BIN_NAME" "$BIN_SRC" || fail "下载 ${BIN_NAME} 失败"
  if ! verify "$BIN_SRC" "$BIN_NAME"; then
    rm -f "$BIN_SRC"
    fail "${BIN_NAME} 未通过官方 SHA256 校验，已丢弃"
  fi
  echo "    SHA256 校验通过"
else
  echo "    使用缓存并校验通过: $BIN_SRC"
fi
file "$BIN_SRC" | sed 's/^/    /'

# ---------- 3. 编译常驻服务 ----------
# 服务只负责配置读写、按间隔调用官方二进制、回显状态；
# DNS 同步逻辑全在官方二进制里。本应用没有网页界面，根路径返回 JSON。
echo "==> 编译常驻服务 (linux/${ARCH})"
WEBSRC=$(mktemp -d)
trap 'rm -rf "$STAGE" "$WEBSRC"' EXIT
cp app/web/main.go "$WEBSRC/"
printf 'module cfddns-web\n\ngo 1.24\n' >"$WEBSRC/go.mod"
(
  cd "$WEBSRC"
  go vet ./...
  CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" \
    go build -trimpath -ldflags "-s -w" -o cfddns-web .
)

# ---------- 4. 组装待打包目录 ----------
# app/ 下的内容会原样进入 target/：
#   app/cfddns/cfddns    官方二进制
#   app/web/cfddns-web   常驻服务
#   app/ui/              保持为空：没有 ui/config 就不会生成「打开」按钮
#
# 说明：应用中心依据 ui/config 的入口声明生成打开按钮。本应用没有网页界面，
# 因此不放该文件，安装后只提供「禁用 / 启动」操作 —— 与同样没有界面的第三方
# 应用 EasyTier 一致（保留空的 ui/ 目录）。系统内置的 trim.text-editor 虽然
# 也带 noDisplay，但因声明了入口，仍然会显示打开按钮。
echo "==> 组装待打包目录"
mkdir -p "$STAGE/app/cfddns" "$STAGE/app/web" "$STAGE/app/ui"
install -m 0755 "$BIN_SRC" "$STAGE/app/cfddns/cfddns"
install -m 0755 "$WEBSRC/cfddns-web" "$STAGE/app/web/cfddns-web"
cp -a app/ui/. "$STAGE/app/ui/" 2>/dev/null || true
cp -r cmd/. "$STAGE/cmd/" && cp -r config/. "$STAGE/config/" && cp -r wizard/. "$STAGE/wizard/"
cp manifest ICON.PNG ICON_256.PNG "$STAGE/"
[ -f LICENSE ] && cp LICENSE "$STAGE/"

# 架构自检：两个二进制都必须是指定架构。
# 这一步专门拦「CI 忘了传 FNOS_GOARCH，两个矩阵项都产出 x86-64」这类问题。
echo "==> 架构自检（期望 ${ARCH}）"
verify_arch "$STAGE/app/cfddns/cfddns" "官方二进制" || exit 1
verify_arch "$STAGE/app/web/cfddns-web" "常驻服务" || exit 1

# 入口自检。
#
# 两点约束来自实测：
#   1. fnpack 强制要求 app/ui/config 存在，缺了会报
#      Required file "app/ui/config" is missing，直接打包失败；
#   2. 应用中心的「打开」按钮取决于 manifest 是否声明
#      desktop_applaunchname —— 声明了才有 appcenter 入口。
# 本应用没有网页界面，因此保留 ui/config 满足打包要求，但不声明
# desktop_applaunchname，应用中心里就只会有「禁用 / 启动」。
if [ ! -f "$STAGE/app/ui/config" ]; then
  echo "    ✗ 缺少 app/ui/config：fnpack 要求该文件存在" >&2
  exit 1
fi
if grep -q '^desktop_applaunchname=' "$STAGE/manifest"; then
  echo "    ✗ manifest 声明了 desktop_applaunchname：无界面应用会多出「打开」按钮" >&2
  exit 1
fi
echo "    ✓ 无 appcenter 入口（应用中心只提供禁用 / 启动）"

# manifest 的 platform 必须与实际编译出的架构一致，fnOS 依据该字段
# 判定兼容性。取值只有 x86 / arm / all。
case "$ARCH" in
  amd64) PLATFORM=x86 ;;
  arm64) PLATFORM=arm ;;
  *) PLATFORM=all ;;
esac
if grep -q '^platform=' "$STAGE/manifest"; then
  sed -i "s/^platform=.*/platform=${PLATFORM}/" "$STAGE/manifest"
else
  echo "platform=${PLATFORM}" >>"$STAGE/manifest"
fi
echo "    manifest platform=${PLATFORM}（对应 GOARCH=${ARCH}）"

echo "    待打包文件:"
( cd "$STAGE" && find . -type f | sort | sed 's/^/      /' )

# ---------- 4. 打包前自检 ----------
echo "==> 打包前自检"
fail=0
check_file() {
  if [ -e "$STAGE/$1" ]; then
    echo "    ✓ $1"
  else
    echo "    ✗ 缺少 $1"
    fail=1
  fi
}
check_exec() {
  if [ -x "$STAGE/$1" ] && [ -f "$STAGE/$1" ]; then
    echo "    ✓ $1 (可执行)"
  elif [ -d "$STAGE/$1" ]; then
    # 这条专门防住线上踩过的坑：路径存在但是目录，运行期 execve 会返回 EACCES
    echo "    ✗ $1 是目录而不是可执行文件"
    fail=1
  else
    echo "    ✗ $1 缺失或不可执行"
    fail=1
  fi
}

for f in manifest ICON.PNG ICON_256.PNG config/privilege config/resource \
         app/cfddns/cfddns; do
  check_file "$f"
done
for f in cmd/main cmd/install_init cmd/install_callback cmd/uninstall_init \
         cmd/uninstall_callback cmd/upgrade_init cmd/upgrade_callback \
         cmd/config_init cmd/config_callback app/web/cfddns-web; do
  check_exec "$f"
done
for f in config/privilege config/resource \
         wizard/install wizard/config wizard/uninstall wizard/upgrade; do
  if python3 -c "import json,sys; json.load(open('$STAGE/$f'))" 2>/dev/null; then
    echo "    ✓ $f (JSON 合法)"
  else
    echo "    ✗ $f JSON 非法"
    fail=1
  fi
done
for k in appname version display_name platform; do
  grep -q "^${k}=" "$STAGE/manifest" || { echo "    ✗ manifest 缺少 $k"; fail=1; }
done

# 图标尺寸与体积
if python3 - "$STAGE" <<'PY'
import os, struct, sys
stage = sys.argv[1]
for path, want in [("ICON.PNG", 64), ("ICON_256.PNG", 256)]:
    f = os.path.join(stage, path)
    b = open(f, "rb").read()
    assert b[:8] == b"\x89PNG\r\n\x1a\n", f"{path} 不是 PNG"
    w, h = struct.unpack(">II", b[16:24])
    assert (w, h) == (want, want), f"{path} 尺寸 {w}x{h}，要求 {want}x{want}"
    kb = len(b) / 1024
    assert kb <= 1024, f"{path} 体积 {kb:.0f}KB 超过 1024KB"
print(f"    ✓ 图标尺寸与体积合规")
PY
then :; else fail=1; fi

if [ "$fail" -ne 0 ]; then
  echo "自检未通过，中止打包" >&2
  exit 1
fi

# ---------- 5. 打包 ----------
if command -v fnpack >/dev/null 2>&1; then
  echo "==> 使用官方 fnpack 打包"
  ( cd "$STAGE" && fnpack build )
  FPK=$(find "$STAGE" -maxdepth 1 -name '*.fpk' | head -n 1)
  if [ -n "$FPK" ]; then
    cp "$FPK" "$OUT/"
    echo "==> 完成"
    ls -l "$OUT"/*.fpk
    exit 0
  fi
  echo "fnpack 未产出 .fpk，回退到开发包" >&2
fi

echo "==> 未找到 fnpack，产出开发包（不能通过应用中心安装）"
DEV="$OUT/${APPNAME}-${VERSION}-${ARCH}-dev.zip"
rm -f "$DEV"
python3 - "$STAGE" "$DEV" <<'PY'
import os, sys, zipfile

stage, out = sys.argv[1], sys.argv[2]
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk(stage):
        for f in sorted(files):
            p = os.path.join(root, f)
            rel = os.path.relpath(p, stage)
            info = zipfile.ZipInfo.from_file(p, rel)   # 保留可执行位
            with open(p, "rb") as src, z.open(info, "w") as dst:
                dst.write(src.read())
print("  已写入:", out)
PY
ls -l "$DEV"
cat <<'EOF'

注意：上面这个是开发包，仅供检视结构，**不能**通过 fnOS 应用中心安装。
要得到可安装的 .fpk，请先安装官方 fnpack：
    https://developer.fnnas.com/docs/cli/fnpack
然后重新执行本脚本。
EOF
