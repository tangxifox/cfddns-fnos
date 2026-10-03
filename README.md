# cfddns-fnos

飞牛 fnOS 应用版 —— 把本机公网 IP 自动同步到 Cloudflare 的 A / AAAA 记录。

**以命令行方式运行**，封装官方 [tangxifox/cfddns](https://github.com/tangxifox/cfddns)
发布的二进制，不自行编译、不修改。打包时会下载对应架构的官方文件并用官方
`SHA256SUMS.txt` 校验，校验不通过直接中止打包。

## 架构

| 二进制 | 来源 | 职责 |
| --- | --- | --- |
| `target/cfddns/cfddns` | **官方发布件**，逐字节一致 | 全部 DNS 同步逻辑：探测公网 IP、读写 Cloudflare 记录 |
| `target/web/cfddns-web` | 本仓库 | 按间隔调用官方二进制、提供配置读写接口与运行状态 |

也就是说，**Cloudflare 相关的判断与请求全部由官方二进制完成**。上游更新同步
逻辑时，替换官方二进制即可，本仓库无需跟着改。

```
应用中心启动
   │
   ▼
target/web/cfddns-web ──按间隔调用──▶ target/cfddns/cfddns --json ──▶ Cloudflare
   │
   └── 监听 Unix Socket，供 fnOS 网关提供接口（应用无网页界面）
```

服务**不监听任何 TCP 端口**，只监听应用目录下的 Unix Socket，由 fnOS 网关鉴权
并转发，不额外暴露网络面。

## 安装

1. 从 [Releases](../../releases/latest) 下载对应架构的 `.fpk`；
2. 在飞牛**应用中心 → 手动安装**中上传；
3. 按向导填写：

   | 向导项 | 说明 |
   | --- | --- |
   | Cloudflare API 令牌 | 在 <https://dash.cloudflare.com/profile/api-tokens> 用 **Edit zone DNS** 模板创建 |
   | 要绑定的域名 | 例如 `example.com`（zone ID 由程序自动查询） |
   | 要更新的记录名 | 例如 `pc`，多个用英文逗号分隔；`@` 表示根域名，`*` 表示泛解析 |
   | 绑定哪类 IP | **IPv4**（A 记录）/ **IPv6**（AAAA 记录）/ 两者都绑定 |

4. 安装后应用自动启动，默认每 5 分钟同步一次。

选择「绑定哪类 IP」时会同时调整记录类型与该协议的探测开关 —— 只改其一都会
造成「不想要的记录也被更新」或「想要的记录没生成」。

## 修改配置

两种方式，任选：

**① 应用中心 → cfddns → 设置** —— 重新填写令牌、域名、记录名，并可切换绑定
的协议类型。留空表示保持原值不变。

**② 直接编辑配置文件** —— 位于应用配置目录的 `config.yaml`
（安装后为 `/vol1/@appconf/cfddns/config.yaml`）：

```yaml
# Cloudflare API 令牌
token: "你的令牌"

# 要绑定的域名
zone: "example.com"

# 要更新的记录：名字 或 名字: 类型（A / AAAA / BOTH）
records:
  - name: pc
    type: BOTH     # A / AAAA / BOTH
    ttl: 1         # 1 = 自动，或 60~86400 秒
  - name: nas
    type: AAAA

# 公网 IP 探测
ipv4:
  enabled: true
  source: auto
ipv6:
  enabled: true
  source: auto
```

改完在应用中心**停止再启动**使配置生效。家庭宽带通常只有公网 IPv6，此时建议
记录类型用 `AAAA` 并把 `ipv4.enabled` 设为 `false`。

需要「部分记录用 IPv6、部分用 IPv4」时，直接编辑配置文件 —— 向导是按记录名
批量替换的，逐条区分请手写 `type` 字段。

## 查看运行状态

本应用没有网页界面，应用中心里只有**禁用 / 启动**操作，**没有打开按钮**。

应用中心的打开按钮取决于 manifest 是否声明 `desktop_applaunchname`：本应用
刻意不声明它，因此不会出现「打开」。同时 `app/ui/config` 仍然保留 ——
官方 fnpack 强制要求该文件存在，缺少时直接打包失败。

服务仍提供 `api/summary` 接口，便于本地排查：返回是否已配置、记录列表、
各协议开关与最近一次同步结果。它只在应用目录下的 Unix Socket 上提供。

```bash
APP=/var/apps/cfddns

# 官方二进制（同步逻辑本体）
$APP/target/cfddns/cfddns status --config $APP/etc/config.yaml   # 令牌脱敏
$APP/target/cfddns/cfddns --config $APP/etc/config.yaml --dry-run
$APP/target/cfddns/cfddns --config $APP/etc/config.yaml --json

# 服务控制
$APP/cmd/main status    # 0=运行 3=未运行
$APP/cmd/main start
$APP/cmd/main stop
```

`--interval` 最短 10 秒（上游为防止触发 Cloudflare 速率限制而设的下限）。

## 从源码构建

```bash
# 需要官方 fnpack：https://developer.fnnas.com/docs/cli/fnpack
./build-fpk.sh                    # x86_64
FNOS_GOARCH=arm64 ./build-fpk.sh  # ARM64
```

脚本依次：生成图标 → 下载官方二进制并按 `SHA256SUMS.txt` 校验 →
编译常驻服务 → 组装待打包目录 → 打包前自检 → `fnpack build` 出包。

自检会拦下：缺少必需文件、`cmd/*` 无执行位、JSON 非法、manifest 缺字段、
误声明 `desktop_applaunchname`、缺少 `app/ui/config`、图标尺寸或体积不合规、
以及「路径存在但是目录」这类只在运行期暴露的问题。

环境变量：`UPSTREAM_VERSION`（上游版本，默认 1.0.0）、
`CFDDNS_RELEASE_URL`（下载前缀）、`CFDDNS_CACHE`（二进制缓存目录）、
`FNOS_GOARCH`、`OUT`。

> 缺少 `fnpack` 时脚本不会伪造 `.fpk`，而是产出 `dist/*-dev.zip` 供检视结构，
> 该文件**不能**通过应用中心安装。

## 测试

```bash
# 向导的 IP 协议处理（v4 / v6 / both / 留空保持原值 / 替换记录名）
bash tool/test-wizard.sh

# 常驻服务的配置解析（四种记录写法、上游 dns: 别名、读写往返、校验）
cd app/web && go test ./...    # 或 bash tool/build-web.sh 一并构建
```

真机建议逐项验证：

1. **安装**：选卷安装 → 走向导（选 IPv6）→ 应用列表可见 → 启动后状态「运行中」
2. **配置**：`cat /vol1/@appconf/cfddns/config.yaml` 应为 `type: AAAA`、
   `ipv6 enabled: true`、`ipv4 enabled: false`
3. **同步**：`target/cfddns/cfddns --dry-run` 能列出记录 → 去掉 `--dry-run`
   后到 Cloudflare 后台确认记录值与本机公网 IP 一致
4. **改设置**：应用中心 → 设置，选 IPv4 保存，配置文件应随之变化
5. **幂等**：连续两次启动不应产生两个进程；`cmd/main status` 返回 0
6. **停止**：`cmd/main stop` 后无残留进程，`status` 返回 3
7. **升级**：安装新版本后配置与日志保留，服务自动拉起
8. **卸载**：选择保留数据后重装，配置应还在

## 目录结构

```
cfddns-fnos/
├── manifest                   应用身份信息
├── ICON.PNG / ICON_256.PNG    应用中心图标
├── config/
│   ├── privilege              运行用户声明（run-as: package）
│   └── resource               资源声明
├── cmd/                       生命周期脚本
│   ├── main                   启动 / 停止 / 状态
│   ├── install_callback       安装后按向导生成配置
│   └── config_callback        设置变更后合并配置并重启
├── wizard/                    安装 / 设置 / 升级 / 卸载向导
├── app/
│   ├── web/main.go            常驻服务源码
│   └── ui/                    入口声明（fnpack 必需，但不声明 applaunchname）
└── tool/
    ├── gen-icons.py           图标生成器（纯标准库）
    ├── install-fnpack.sh      获取官方 fnpack
    ├── build-web.sh           本地编译常驻服务
    └── test-wizard.sh         向导逻辑测试
```

## 许可

Apache-2.0
