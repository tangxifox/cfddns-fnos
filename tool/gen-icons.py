#!/usr/bin/env python3
"""生成 cfddns 应用图标（纯标准库，不需要 Pillow）。

设计：圆角方块底 + 白色云朵，云内嵌一个向下箭头，表达「把地址写进 DNS」。
不使用任何外部素材，全部几何绘制。

实现要点：
  * 用「有符号距离场 + 覆盖率」做抗锯齿，而不是超采样——
    任意形状都能拿到平滑边缘，且不需要图像库。
  * PNG 由 zlib + struct 手写编码（RGBA，无滤波），
    因此本脚本在只装了标准库的 Python 上也能跑。

用法：
    python3 tool/gen-icons.py

输出（相对仓库根目录）：
    ICON.PNG            64x64
    ICON_256.PNG        256x256
    app/ui/images/icon-64.png
    app/ui/images/icon-256.png

fnOS 规范：正方形、sRGB、单文件 ≤ 1024 KB，64px 下仍要辨识得出来，
所以云和箭头都画得足够粗。
"""

from __future__ import annotations

import math
import os
import struct
import sys
import zlib

# 品牌蓝，与 Web UI 主色一致
BG_TOP = (77, 126, 248)
BG_BOTTOM = (58, 102, 220)
FG = (255, 255, 255)


# ---------------------------------------------------------------- 形状（SDF）

def sd_round_rect(px: float, py: float, cx: float, cy: float,
                  hw: float, hh: float, r: float) -> float:
    """圆角矩形的有符号距离：<0 在内部。"""
    qx = abs(px - cx) - (hw - r)
    qy = abs(py - cy) - (hh - r)
    ax, ay = max(qx, 0.0), max(qy, 0.0)
    return math.hypot(ax, ay) + min(max(qx, qy), 0.0) - r


def sd_circle(px: float, py: float, cx: float, cy: float, r: float) -> float:
    return math.hypot(px - cx, py - cy) - r


def sd_triangle(px: float, py: float, a, b, c) -> float:
    """三角形的精确 SDF（含符号），用于抗锯齿的箭头。"""
    def edge(p, q):
        return (q[0] - p[0]) * (py - p[1]) - (q[1] - p[1]) * (px - p[0])

    d1, d2, d3 = edge(a, b), edge(b, c), edge(c, a)
    has_neg = d1 < 0 or d2 < 0 or d3 < 0
    has_pos = d1 > 0 or d2 > 0 or d3 > 0
    if not (has_neg and has_pos):
        return -min(abs(d1), abs(d2), abs(d3)) / math.hypot(b[0] - a[0], b[1] - a[1])
    # 外部：到三条边的最小距离
    def seg_dist(p, q):
        vx, vy = q[0] - p[0], q[1] - p[1]
        wx, wy = px - p[0], py - p[1]
        t = 0.0 if (vx * vx + vy * vy) == 0 else max(0.0, min(1.0, (wx * vx + wy * vy) / (vx * vx + vy * vy)))
        return math.hypot(wx - t * vx, wy - t * vy)

    return min(seg_dist(a, b), seg_dist(b, c), seg_dist(c, a))


def coverage(sd: float) -> float:
    """把有符号距离转成 0..1 覆盖率，边缘 1px 内平滑过渡。"""
    return min(1.0, max(0.0, 0.5 - sd))


def union_sd(*vals: float) -> float:
    """并集：取最小距离。"""
    return min(vals)


# ---------------------------------------------------------------- 图形拼装

def mix(a, b, t):
    return tuple(int(round(a[i] + (b[i] - a[i]) * t)) for i in range(3))


def cloud_sdf(px: float, py: float, s: float):
    """云朵 = 三个圆 + 一个圆角矩形底的并集。s 为画布边长。"""
    cx, cy = s * 0.5, s * 0.40
    w = s * 0.66
    y_base = cy + w * 0.10

    parts = [
        sd_circle(px, py, cx - w * 0.34, y_base - w * 0.15, w * 0.17),
        sd_circle(px, py, cx + w * 0.02, y_base - w * 0.21, w * 0.24),
        sd_circle(px, py, cx + w * 0.29, y_base - w * 0.13, w * 0.19),
        sd_round_rect(px, py, cx - w * 0.02, y_base - w * 0.02,
                      w * 0.36, w * 0.10, w * 0.10),
    ]
    return union_sd(*parts)


def arrow_sdf(px: float, py: float, s: float):
    """向下箭头 = 圆角箭杆 + 三角箭头的并集。"""
    cx = s * 0.5
    h = s * 0.34
    top = s * 0.68 - h / 2
    shaft_w = h * 0.36
    head_w = h * 0.86
    head_h = h * 0.42
    shaft_bottom = s * 0.68 + h / 2 - head_h

    shaft = sd_round_rect(px, py, cx, (top + shaft_bottom) / 2,
                          shaft_w / 2, (shaft_bottom - top) / 2, shaft_w / 2)
    head = sd_triangle(
        px, py,
        (cx - head_w / 2, shaft_bottom),
        (cx + head_w / 2, shaft_bottom),
        (cx, shaft_bottom + head_h),
    )
    return union_sd(shaft, head)


def render(size: int) -> bytes:
    """渲染成 RGBA 像素数据（逐行、无滤波）。"""
    s = float(size)
    rows = []
    for y in range(size):
        py = y + 0.5
        t = y / max(1, size - 1)
        bg = mix(BG_TOP, BG_BOTTOM, t)
        row = bytearray()
        for x in range(size):
            px = x + 0.5

            # 底：圆角方块
            a_bg = coverage(sd_round_rect(px, py, s / 2, s / 2, s / 2, s / 2, s * 0.22))
            r, g, b = bg

            # 前景：云 ∪ 箭头
            a_fg = coverage(union_sd(cloud_sdf(px, py, s), arrow_sdf(px, py, s)))
            if a_fg > 0:
                r = int(round(r + (FG[0] - r) * a_fg))
                g = int(round(g + (FG[1] - g) * a_fg))
                b = int(round(b + (FG[2] - b) * a_fg))

            row += bytes((r, g, b, int(round(a_bg * 255))))
        rows.append(bytes(row))
    return b"".join(rows)


# ---------------------------------------------------------------- PNG 编码

def write_png(path: str, size: int, rgba: bytes) -> None:
    def chunk(tag: bytes, data: bytes) -> bytes:
        return (struct.pack(">I", len(data)) + tag + data
                + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF))

    raw = bytearray()
    stride = size * 4
    for y in range(size):
        raw.append(0)  # 滤波类型 0（None）
        raw += rgba[y * stride:(y + 1) * stride]

    ihdr = struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0)  # 8bit RGBA
    png = (b"\x89PNG\r\n\x1a\n"
           + chunk(b"IHDR", ihdr)
           + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
           + chunk(b"IEND", b""))

    tmp = path + ".tmp"
    with open(tmp, "wb") as f:
        f.write(png)
    os.replace(tmp, path)


def main() -> int:
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    targets = [
        (64, os.path.join(root, "ICON.PNG")),
        (256, os.path.join(root, "ICON_256.PNG")),
        # 桌面入口图标位于界面目录的 images/ 下，由 app/web/ui/config 的
        # icon 字段引用（{0} 由系统替换成 64 或 256）
        (64, os.path.join(root, "app", "ui", "images", "icon_64.png")),
        (256, os.path.join(root, "app", "ui", "images", "icon_256.png")),
    ]

    for size, path in targets:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        write_png(path, size, render(size))
        kb = os.path.getsize(path) / 1024
        flag = "  ← 超过 1024KB 限制!" if kb > 1024 else ""
        print(f"  {os.path.relpath(path, root):<38} {size:>3}x{size:<3} {kb:7.1f} KB{flag}")
        if kb > 1024:
            return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
