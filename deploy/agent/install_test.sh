#!/bin/sh
# 验证 install.sh：参数校验、校验和失败中止、调用顺序、目录 0700、令牌不外泄、unit 与 spark-agent.service 一致、--cluster-iface 的 NM 配置。
# 用假的 curl / sha256sum / install / systemctl / nmcli / id / uname / spark-agent，路径指向临时目录；不需要 root。
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
sh -n "$HERE/install.sh"
FAKE="$(mktemp -d)"
trap 'rm -rf "$FAKE"' EXIT
LOG="$FAKE/calls.log"; : > "$LOG"
mkdir -p "$FAKE/bin"
B="$FAKE/bin"

cat > "$B/id" <<EOF2
#!/bin/sh
echo "\${FAKE_UID:-0}"
EOF2
cat > "$B/uname" <<EOF2
#!/bin/sh
echo aarch64
EOF2
# curl：把假 agent 脚本写到 -o 指定文件
cat > "$FAKE/agent.src" <<EOF2
#!/bin/sh
echo "spark-agent \$*" >> "$LOG"
[ "\${FAKE_SELFCHECK_FAIL:-}" = 1 ] && [ "\$1" = selfcheck ] && exit 1
exit 0
EOF2
cat > "$B/curl" <<EOF2
#!/bin/sh
echo "curl \$*" >> "$LOG"
while [ \$# -gt 0 ]; do [ "\$1" = -o ] && out="\$2"; shift; done
cp "$FAKE/agent.src" "\$out"
EOF2
cat > "$B/sha256sum" <<EOF2
#!/bin/sh
echo "deadbeef  \$1"
EOF2
# install：忽略 -o/-g（非 root），只做 -d 与拷贝+chmod，并记录
cat > "$B/install" <<EOF2
#!/bin/sh
echo "install \$*" >> "$LOG"
mode=""; dir=0
while [ \$# -gt 0 ]; do
  case "\$1" in -m) mode="\$2"; shift 2 ;; -o|-g) shift 2 ;; -d) dir=1; shift ;; *) break ;; esac
done
if [ \$dir = 1 ]; then for d in "\$@"; do mkdir -p "\$d"; chmod "\$mode" "\$d"; done
else cp "\$1" "\$2"; chmod "\$mode" "\$2"; fi
EOF2
cat > "$B/nmcli" <<EOF2
#!/bin/sh
echo "nmcli \$*" >> "$LOG"
EOF2
cat > "$B/systemctl" <<EOF2
#!/bin/sh
echo "systemctl \$*" >> "$LOG"
EOF2
chmod +x "$B"/*

export SPARK_INSTALL_BIN_PATH="$FAKE/usr/spark-agent"
export SPARK_INSTALL_CONF_DIR="$FAKE/etc"
export SPARK_INSTALL_STATE_DIR="$FAKE/var"
export SPARK_INSTALL_UNIT_PATH="$FAKE/unit"
export SPARK_INSTALL_NM_CONF="$FAKE/nm/conf.d/99-spark-cluster.conf"
export SPARK_INSTALL_IP_FORWARD="$FAKE/ip_forward"
export SPARK_INSTALL_NMCLI="$B/nmcli" # 绝不调用真机的 nmcli
echo 1 > "$FAKE/ip_forward"
mkdir -p "$FAKE/usr"
run() { PATH="$B:$PATH" sh "$HERE/install.sh" "$@"; }
OKARGS="--platform x:1 --token spk1_SECRET --ca-sha256 ca --binary-sha256 deadbeef --download-url http://f"

# 缺参数 / 缺下载地址 / 缺取值 / 非 root / 校验失败
if run --platform x:1 2>/dev/null; then echo "缺参数应失败"; exit 1; fi
if run --platform x:1 --token spk1_t --ca-sha256 ca 2>/dev/null; then echo "缺下载地址应失败"; exit 1; fi
if run --platform x:1 --token 2>/dev/null; then echo "缺取值应失败"; exit 1; fi
# shellcheck disable=SC2086
if (export FAKE_UID=1000; run $OKARGS) 2>/dev/null; then echo "非 root 应失败"; exit 1; fi
if run --platform x:1 --token spk1_t --ca-sha256 ca --binary-sha256 wrong --download-url http://f >/dev/null 2>&1; then echo "校验失败应中止"; exit 1; fi
[ ! -e "$SPARK_INSTALL_BIN_PATH" ] || { echo "校验失败不应安装二进制"; exit 1; }
[ ! -e "$FAKE/unit" ] || { echo "校验失败不应写 unit"; exit 1; }
# selfcheck 失败：中止，且不安装/启用 unit
: > "$LOG"
# shellcheck disable=SC2086
if (export FAKE_SELFCHECK_FAIL=1; run $OKARGS) >/dev/null 2>&1; then echo "selfcheck 失败应中止"; exit 1; fi
! grep -q systemctl "$LOG" || { echo "selfcheck 失败不应调用 systemctl"; exit 1; }
! grep -q "spark-agent enroll" "$LOG" || { echo "selfcheck 失败时不应 enroll（令牌会被白白消耗）"; exit 1; }
! grep -q "install -d" "$LOG" || { echo "selfcheck 失败不应创建目录"; exit 1; }
[ ! -e "$SPARK_INSTALL_BIN_PATH" ] || { echo "selfcheck 失败不应替换二进制"; exit 1; }
[ -z "$(ls -A "$FAKE/usr")" ] || { echo "selfcheck 失败应清理临时文件"; exit 1; }
[ ! -e "$FAKE/unit" ] || { echo "selfcheck 失败不应写 unit"; exit 1; }

# 成功路径；预置过宽权限的目录，验证重跑会修正为 700
rm -rf "$FAKE/etc" "$FAKE/var"; mkdir -p "$FAKE/etc" "$FAKE/var"; chmod 755 "$FAKE/etc" "$FAKE/var"
: > "$LOG"
# shellcheck disable=SC2086
run $OKARGS > "$FAKE/out.txt" 2>&1 || { cat "$FAKE/out.txt"; exit 1; }
# 幂等：再跑一次
# shellcheck disable=SC2086
run $OKARGS >> "$FAKE/out.txt" 2>&1
! grep -q SECRET "$FAKE/out.txt" || { echo "令牌不应出现在脚本输出"; exit 1; }
grep -q "curl -fsSL http://f/spark-agent-linux-arm64" "$LOG"
grep -q "spark-agent enroll --platform x:1 --token spk1_SECRET --ca-sha256 ca --dir $FAKE/etc" "$LOG"
grep -q "install -d -m 0700 -o root -g root $FAKE/etc $FAKE/var" "$LOG"
grep -q "spark-agent selfcheck --state-dir $FAKE/var" "$LOG"
grep -q "systemctl enable spark-agent" "$LOG"
grep -q "systemctl restart spark-agent" "$LOG"
for d in etc var; do
  case "$(ls -ld "$FAKE/$d")" in drwx------*) ;; *) echo "$d 应为 0700"; exit 1 ;; esac
done
# 顺序：selfcheck < enroll < systemctl enable（取第一轮）
l() { grep -n "$1" "$LOG" | head -1 | cut -d: -f1; }
[ "$(l 'spark-agent selfcheck')" -lt "$(l 'spark-agent enroll')" ]
[ "$(l 'spark-agent enroll')" -lt "$(l 'systemctl enable')" ]
[ -x "$SPARK_INSTALL_BIN_PATH" ] && [ "$(ls -A "$FAKE/usr")" = "spark-agent" ] || { echo "二进制应已安装且无临时文件残留"; exit 1; }
# unit 内容与仓库里的 unit 文件一致，且满足要求
cmp "$FAKE/unit" "$HERE/spark-agent.service"
for k in Restart=always RestartSec=3 User=root OOMScoreAdjust=-900 After=docker.service Wants=docker.service; do
  grep -q "$k" "$FAKE/unit" || { echo "unit 缺 $k"; exit 1; }
done
! grep -q -- '--fake' "$FAKE/unit" || { echo "unit 不应含 --fake"; exit 1; }
# 不带 --cluster-iface：不写 NM 配置、不调用 nmcli；ip_forward=1 时不提示
[ ! -e "$SPARK_INSTALL_NM_CONF" ] || { echo "未给 --cluster-iface 不应写 NM 配置"; exit 1; }
! grep -q nmcli "$LOG" || { echo "未给 --cluster-iface 不应调用 nmcli"; exit 1; }
grep -q "CX-7 口不要配置 IP" "$FAKE/out.txt" || { echo "缺 CX-7 口勿配 IP 提示"; exit 1; }
! grep -q "ip_forward 当前为" "$FAKE/out.txt" || { echo "ip_forward=1 不应提示"; exit 1; }

# --cluster-iface（可重复、去重）：写 unmanaged-devices+= 并 nmcli general reload，且在自检之前
expect_conf() {
  printf '%s\n' "# 由 spark-agent install.sh --cluster-iface 生成：集群网卡（CX-7 口）不归 NetworkManager 管理，避免 NM 给它们加地址。" \
    "[keyfile]" "unmanaged-devices+=$1" > "$FAKE/want.conf"
  cmp "$SPARK_INSTALL_NM_CONF" "$FAKE/want.conf" || { echo "NM 配置内容不对:"; cat "$SPARK_INSTALL_NM_CONF"; exit 1; }
}
echo 0 > "$FAKE/ip_forward"
: > "$LOG"
# shellcheck disable=SC2086
run $OKARGS --cluster-iface enp1s0f1np1 --cluster-iface enP2p1s0f1np1 --cluster-iface enp1s0f1np1 > "$FAKE/out.txt" 2>&1 || { cat "$FAKE/out.txt"; exit 1; }
expect_conf "interface-name:enp1s0f1np1;interface-name:enP2p1s0f1np1"
case "$(ls -l "$SPARK_INSTALL_NM_CONF")" in -rw-r--r--*) ;; *) echo "NM 配置应为 0644"; exit 1 ;; esac
[ -z "$(find "$FAKE/nm" -name '*.tmp')" ] || { echo "不应残留临时文件"; exit 1; }
[ "$(grep -c 'nmcli general reload' "$LOG")" = 1 ] || { echo "应 nmcli general reload 一次"; cat "$LOG"; exit 1; }
[ "$(l 'nmcli general reload')" -lt "$(l 'spark-agent selfcheck')" ] || { echo "NM 配置应在自检之前写入"; exit 1; }
grep -q "ip_forward 当前为 0" "$FAKE/out.txt" || { echo "ip_forward=0 应提示"; cat "$FAKE/out.txt"; exit 1; }

# 只给 --cluster-iface：整体替换列表、reload，不下载 / 不接入 / 不动 systemd
: > "$LOG"
run --cluster-iface enp1s0f0np0 > "$FAKE/out.txt" 2>&1 || { cat "$FAKE/out.txt"; exit 1; }
expect_conf "interface-name:enp1s0f0np0"
grep -q "nmcli general reload" "$LOG"
! grep -Eq "curl|spark-agent|systemctl|install -d" "$LOG" || { echo "只给 --cluster-iface 不应安装 / 接入"; cat "$LOG"; exit 1; }
# 没有 nmcli：照样写配置，跳过 reload 并提示
rm -f "$SPARK_INSTALL_NM_CONF"
(export SPARK_INSTALL_NMCLI="$FAKE/no-such-nmcli"; run --cluster-iface enp1s0f0np0) > "$FAKE/out.txt" 2>&1 || { cat "$FAKE/out.txt"; exit 1; }
expect_conf "interface-name:enp1s0f0np0"
grep -q "未找到 nmcli" "$FAKE/out.txt" || { echo "无 nmcli 应提示"; exit 1; }
# 非法网卡名 / 缺取值 / 非 root：失败且不改配置
for bad in "a;b" "a b" "-x" ".x" "_x" "abcdefghijklmnop" "" 'x$(id)'; do
  if run --cluster-iface "$bad" >/dev/null 2>&1; then echo "非法网卡名应失败: $bad"; exit 1; fi
done
if run --cluster-iface 2>/dev/null; then echo "--cluster-iface 缺取值应失败"; exit 1; fi
if (export FAKE_UID=1000; run --cluster-iface enp1s0f1np1) 2>/dev/null; then echo "非 root 应失败"; exit 1; fi
expect_conf "interface-name:enp1s0f0np0"
echo "install.sh ok"
