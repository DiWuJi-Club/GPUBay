#!/bin/sh
# 验证 install.sh：参数校验、校验和失败中止、调用顺序、目录 0700、令牌不外泄、unit 与 spark-agent.service 一致。
# 用假的 curl / sha256sum / install / systemctl / id / uname / spark-agent，路径指向临时目录；不需要 root。
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
cat > "$B/systemctl" <<EOF2
#!/bin/sh
echo "systemctl \$*" >> "$LOG"
EOF2
chmod +x "$B"/*

export SPARK_INSTALL_BIN_PATH="$FAKE/usr/spark-agent"
export SPARK_INSTALL_CONF_DIR="$FAKE/etc"
export SPARK_INSTALL_STATE_DIR="$FAKE/var"
export SPARK_INSTALL_UNIT_PATH="$FAKE/unit"
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
# 顺序：enroll < selfcheck < systemctl enable（取第一轮）
l() { grep -n "$1" "$LOG" | head -1 | cut -d: -f1; }
[ "$(l 'spark-agent enroll')" -lt "$(l 'spark-agent selfcheck')" ]
[ "$(l 'spark-agent selfcheck')" -lt "$(l 'systemctl enable')" ]
# unit 内容与仓库里的 unit 文件一致，且满足要求
cmp "$FAKE/unit" "$HERE/spark-agent.service"
for k in Restart=always RestartSec=3 User=root OOMScoreAdjust=-900 After=docker.service Wants=docker.service; do
  grep -q "$k" "$FAKE/unit" || { echo "unit 缺 $k"; exit 1; }
done
! grep -q -- '--fake' "$FAKE/unit" || { echo "unit 不应含 --fake"; exit 1; }
echo "install.sh ok"
