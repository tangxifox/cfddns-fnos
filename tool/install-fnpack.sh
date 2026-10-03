#!/usr/bin/env bash
# 安装官方 fnpack 到 /usr/local/bin。
#
# 用法:
#   ./tool/install-fnpack.sh             # 自行抓取文档页找最新链接
#   ./tool/install-fnpack.sh page.html   # 用已下载的文档页
#
# 为什么需要这个脚本而不是在 workflow 里直接写：
#   1. 官方文件名带版本号（fnpack-x.y.z-linux-amd64），必须动态获取；
#   2. 必须在 CI 里失败得明确 —— 早期版本的判断写成
#      `head -c 4 | grep -q ELF`，而 ELF 魔数是 7f 45 4c 46，
#      文本里并不含 "ELF"，于是 404 返回的 HTML 也被当成二进制装上，
#      构建静默降级成开发包却仍然显示成功。
set -euo pipefail

ARCH="${FNOS_FNPACK_ARCH:-amd64}"
DEST="${FNPACK_DEST:-/usr/local/bin/fnpack}"
DOC="${1:-}"

verify_elf() {
  [ -s "$1" ] || return 1
  # ELF 魔数：7f 45 4c 46
  [ "$(head -c 4 "$1" | od -An -tx1 | tr -d ' \n')" = "7f454c46" ]
}

if [ -z "$DOC" ]; then
  DOC=$(mktemp)
  echo "==> 抓取文档页以获取最新下载地址"
  if ! curl -fsSL --max-time 30 "https://developer.fnnas.com/docs/cli/fnpack/" -o "$DOC"; then
    echo "    文档页抓取失败，将只使用内置的已知版本" >&2
    : >"$DOC"
  fi
fi

# 先取文档页里当前公布的链接，再兜底到已知版本
urls=$(grep -oE "https://[^\"'<> ]*fnpack-[0-9.]+-linux-${ARCH}" "$DOC" 2>/dev/null | sort -u || true)
urls="${urls}
https://static2.fnnas.com/fnpack/fnpack-1.2.3-linux-${ARCH}"

ok=0
while IFS= read -r url; do
  [ -n "$url" ] || continue
  echo "==> 尝试 $url"
  if curl -fsSL --max-time 90 "$url" -o /tmp/fnpack.dl && verify_elf /tmp/fnpack.dl; then
    install -m 0755 /tmp/fnpack.dl "$DEST"
    if "$DEST" --help >/dev/null 2>&1; then
      echo "    安装成功: $DEST"
      ok=1
      break
    fi
    echo "    下载到了可执行文件但运行失败，继续尝试" >&2
  else
    echo "    该地址不可用或返回的不是可执行文件" >&2
  fi
done <<<"$urls"

if [ "$ok" -ne 1 ]; then
  echo "未能获取官方 fnpack，构建将只能产出开发包（不能在应用中心安装）。" >&2
  echo "下载地址见 https://developer.fnnas.com/docs/cli/fnpack" >&2
  exit 1
fi
