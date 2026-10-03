#!/usr/bin/env bash
# 测试 install_callback / config_callback 对 IP 协议的处理。
#
# 用法: bash tool/test-wizard.sh
#
# 用与真机一致的目录布局（<root>/var/apps/cfddns/{cmd,etc,target,var,tmp}），
# 因为 config_callback 是由 TRIM_APPDEST 反推 cmd/main 位置的。
set -euo pipefail

cd "$(dirname "$0")/.."
FAILED=0

# 为每个用例准备一份干净的安装目录
setup() {
  local root="$1"
  rm -rf "$root"
  mkdir -p "$root/var/apps/cfddns/etc" "$root/var/apps/cfddns/var" \
           "$root/var/apps/cfddns/tmp" "$root/var/apps/cfddns/target" \
           "$root/var/apps/cfddns/cmd"
  cp cmd/* "$root/var/apps/cfddns/cmd/"
  chmod +x "$root/var/apps/cfddns/cmd/"*
}

# 调用一个回调；环境变量由调用方通过 env 传入
run_cb() {
  local root="$1" script="$2"
  shift 2
  env -i PATH="$PATH" HOME="$HOME" \
    TRIM_APPDEST="$root/var/apps/cfddns/target" \
    TRIM_PKGVAR="$root/var/apps/cfddns/var" \
    TRIM_PKGETC="$root/var/apps/cfddns/etc" \
    TRIM_PKGTMP="$root/var/apps/cfddns/tmp" \
    TRIM_TEMP_LOGFILE="$root/wizard.log" \
    "$@" \
    bash "$root/var/apps/cfddns/cmd/$script"
}

# 只匹配非注释行：配置文件里带 # 的示例写法不应影响断言结果
grep_real() { grep -iE "$1" "$2" | grep -v '^[[:space:]]*#'; }

check() {
  local desc="$1" file="$2" pattern="$3"
  if [ -n "$(grep_real "$pattern" "$file")" ]; then
    echo "    ✓ $desc"
  else
    echo "    ✗ $desc"
    echo "      期望匹配: $pattern"
    sed 's/^/      | /' "$file"
    FAILED=1
  fi
}

check_absent() {
  local desc="$1" file="$2" pattern="$3"
  if [ -n "$(grep_real "$pattern" "$file")" ]; then
    echo "    ✗ $desc（不应出现）"
    sed 's/^/      | /' "$file"
    FAILED=1
  else
    echo "    ✓ $desc"
  fi
}

SHOW=0
[ "${1:-}" = "-v" ] && SHOW=1

echo "==> 安装向导：wizard_ip_mode 的三种取值"
for mode in v4 v6 both; do
  R=$(mktemp -d)
  setup "$R"
  run_cb "$R" install_callback \
    wizard_token=cfat_TEST_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
    wizard_zone=example.com wizard_records=pc,nas wizard_ip_mode="$mode"
  F="$R/var/apps/cfddns/etc/config.yaml"
  echo "  --- $mode ---"
  case "$mode" in
    v4)
      check "记录类型为 a" "$F" '^[[:space:]]*type: a$'
      check_absent "记录类型不含 aaaa" "$F" 'type: aaaa'
      check "ipv4 enabled" "$F" '^  enabled: true$'
      ;;
    v6)
      check "记录类型为 aaaa" "$F" '^[[:space:]]*type: aaaa$'
      check "ipv6 已启用" "$F" 'enabled: true'
      ;;
    both)
      check "记录类型为 both" "$F" '^[[:space:]]*type: both$'
      ;;
  esac
  # 逐条检查 ipv4/ipv6 段的 enabled
  EN4=$(sed -n '/^ipv4:/,/^ipv6:/p' "$F" | sed -n 's/^[[:space:]]*enabled:[[:space:]]*//p' | head -1)
  EN6=$(sed -n '/^ipv6:/,$p' "$F" | sed -n 's/^[[:space:]]*enabled:[[:space:]]*//p' | head -1)
  case "$mode" in
    v4)   [ "$EN4" = true ] && [ "$EN6" = false ] || { echo "    ✗ ipv4/ipv6 开关应为 true/false，实际 $EN4/$EN6"; FAILED=1; } ;;
    v6)   [ "$EN4" = false ] && [ "$EN6" = true ] || { echo "    ✗ ipv4/ipv6 开关应为 false/true，实际 $EN4/$EN6"; FAILED=1; } ;;
    both) [ "$EN4" = true ] && [ "$EN6" = true ] || { echo "    ✗ ipv4/ipv6 开关应为 true/true，实际 $EN4/$EN6"; FAILED=1; } ;;
  esac
  [ "$SHOW" = 1 ] && sed 's/^/      | /' "$F"
  rm -rf "$R"
done

echo
echo "==> 设置向导：留空必须保持原设置，不能被翻回双栈"
R=$(mktemp -d)
setup "$R"
run_cb "$R" install_callback \
  wizard_token=cfat_TEST_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
  wizard_zone=example.com wizard_records=pc wizard_ip_mode=v6
F="$R/var/apps/cfddns/etc/config.yaml"
echo "  初始（v6）:"; [ "$SHOW" = 1 ] && sed 's/^/    | /' "$F"

# 只改令牌，其余留空
run_cb "$R" config_callback \
  wizard_token=cfat_NEW_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
  wizard_zone= wizard_records= wizard_ip_mode=
echo "  只改令牌后:"
check "令牌已更新" "$F" 'cfat_NEW_'
check "记录类型仍为 aaaa" "$F" '^[[:space:]]*type: aaaa$'
EN4=$(sed -n '/^ipv4:/,/^ipv6:/p' "$F" | sed -n 's/^[[:space:]]*enabled:[[:space:]]*//p' | head -1)
EN6=$(sed -n '/^ipv6:/,$p' "$F" | sed -n 's/^[[:space:]]*enabled:[[:space:]]*//p' | head -1)
if [ "$EN4" = false ] && [ "$EN6" = true ]; then
  echo "    ✓ ipv4/ipv6 开关保持 false/true"
else
  echo "    ✗ 开关被改动：$EN4/$EN6（应为 false/true）"
  FAILED=1
fi
[ "$SHOW" = 1 ] && sed 's/^/    | /' "$F"

echo
echo "==> 设置向导：切换模式应同时改记录类型与开关"
run_cb "$R" config_callback wizard_ip_mode=v4
echo "  切到 v4 后:"
check "记录类型变为 a" "$F" '^[[:space:]]*type: a$'
EN4=$(sed -n '/^ipv4:/,/^ipv6:/p' "$F" | sed -n 's/^[[:space:]]*enabled:[[:space:]]*//p' | head -1)
EN6=$(sed -n '/^ipv6:/,$p' "$F" | sed -n 's/^[[:space:]]*enabled:[[:space:]]*//p' | head -1)
if [ "$EN4" = true ] && [ "$EN6" = false ]; then
  echo "    ✓ ipv4/ipv6 开关变为 true/false"
else
  echo "    ✗ 开关错误：$EN4/$EN6（应为 true/false）"
  FAILED=1
fi

echo
echo "==> 设置向导：替换记录名，未指定协议时用 BOTH（由全局开关控制），并丢弃旧记录的 TTL"
printf 'token: "t"\nzone: "example.com"\nrecords:\n  - name: old\n    type: AAAA\n    ttl: 300\nipv4:\n  enabled: false\n  source: auto\nipv6:\n  enabled: true\n  source: auto\n' > "$F"
run_cb "$R" config_callback wizard_records=web,mail
echo "  替换记录名后:"
check "包含 web" "$F" 'name: web'
check "包含 mail" "$F" 'name: mail'
check_absent "不再包含 old" "$F" 'name: old'
check "类型为 BOTH" "$F" '^[[:space:]]*type: BOTH$'
check_absent "旧记录的 ttl 300 未被带过来" "$F" 'ttl: 300'

echo
echo "==> 设置向导：同时指定记录名与协议时，类型按所选协议"
run_cb "$R" config_callback wizard_records=svc wizard_ip_mode=v6
echo "  records=svc + ip_mode=v6 后:"
check "包含 svc" "$F" 'name: svc'
check "类型为 AAAA" "$F" '^[[:space:]]*type: AAAA$'

echo
echo "==> 不填记录名时不应凭空生成记录（旧行为会默认写入 pc）"
R2=$(mktemp -d)
setup "$R2"
run_cb "$R2" install_callback \
  wizard_token=cfat_TEST_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
  wizard_zone=example.com wizard_records= wizard_ip_mode=v6
F2="$R2/var/apps/cfddns/etc/config.yaml"
echo "  记录段: $(grep -A1 '^records:' "$F2" | head -2 | tr '\n' ' ')"
check "记录段为空列表" "$F2" '^records: \[\]$'
check_absent "未生成 pc 记录" "$F2" 'name: pc'
rm -rf "$R2"

echo
echo "==> 设置向导：无任何输入时应保持配置不变"
BEFORE=$(cat "$F")
run_cb "$R" config_callback
AFTER=$(cat "$F")
if [ "$BEFORE" = "$AFTER" ]; then
  echo "    ✓ 配置未被改动"
else
  echo "    ✗ 配置被改动了"
  diff <(echo "$BEFORE") <(echo "$AFTER") | sed 's/^/      /'
  FAILED=1
fi
rm -rf "$R"

echo
if [ "$FAILED" = 0 ]; then
  echo "全部通过"
else
  echo "存在失败项" >&2
  exit 1
fi
