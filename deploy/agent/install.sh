#!/bin/sh
# Spark Cloud 节点 Agent 安装脚本：下载并校验二进制 -> selfcheck(用临时二进制，先于 enroll) -> 原子替换二进制 -> 目录(0700) -> enroll -> systemd。
# 用法（后台「生成接入命令」给出）：
#   curl -fsSL <URL>/install.sh | sudo sh -s -- --platform host:9443 --token spk1_... --ca-sha256 <fp> --binary-sha256 <sha> --download-url <URL>
# 可重复执行（升级 / 重新接入）。令牌只作为 enroll 的参数传递，脚本不打印它。
# 注意：curl | sudo sh 由 /bin/sh（Ubuntu 上是 dash）执行，dash 没有 pipefail，所以只在支持时启用；脚本本身不使用管道（没有 pipefail 也不受影响）。
set -eu
# shellcheck disable=SC3040
(set -o pipefail) 2>/dev/null && set -o pipefail

PLATFORM=""; TOKEN=""; CA_SHA=""; BIN_SHA=""; URL="${SPARK_AGENT_DOWNLOAD_URL:-}"
BIN_NAME="spark-agent-linux-arm64"
# 以下路径只为让脚本测试能在临时目录运行；真机不要设置。
BIN_PATH="${SPARK_INSTALL_BIN_PATH:-/usr/local/bin/spark-agent}"
CONF_DIR="${SPARK_INSTALL_CONF_DIR:-/etc/spark-agent}"
STATE_DIR="${SPARK_INSTALL_STATE_DIR:-/var/lib/spark-agent}"
UNIT="${SPARK_INSTALL_UNIT_PATH:-/etc/systemd/system/spark-agent.service}"

die() { echo "错误: $*" >&2; exit 1; }

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --platform|--token|--ca-sha256|--binary-sha256|--download-url)
        [ $# -ge 2 ] || die "参数 $1 缺少取值" ;;
    esac
    case "$1" in
      --platform) PLATFORM="$2"; shift 2 ;;
      --token) TOKEN="$2"; shift 2 ;;
      --ca-sha256) CA_SHA="$2"; shift 2 ;;
      --binary-sha256) BIN_SHA="$2"; shift 2 ;;
      --download-url) URL="$2"; shift 2 ;;
      *) die "未知参数: $1" ;;
    esac
  done
  [ -n "$PLATFORM" ] && [ -n "$TOKEN" ] && [ -n "$CA_SHA" ] || die "--platform --token --ca-sha256 都必填"
  [ -n "$URL" ] || die "不知道从哪里下载二进制：请传 --download-url（后台生成的命令自带）"
  [ "$(id -u)" = "0" ] || die "请用 root 执行（sudo）"
  [ "$(uname -m)" = "aarch64" ] || die "只支持 arm64（aarch64），当前 $(uname -m)"
  [ -n "$BIN_SHA" ] || echo "警告: 未提供 --binary-sha256，跳过二进制校验" >&2

  # 临时文件放在目标目录：selfcheck 可直接执行（/tmp 可能 noexec），且 mv 是同一文件系统内的原子替换
  TMP="$(mktemp "$(dirname "$BIN_PATH")/.spark-agent.XXXXXX")" || die "无法在 $(dirname "$BIN_PATH") 创建临时文件"
  trap 'rm -f "$TMP"' EXIT
  echo "下载 $URL/$BIN_NAME ..."
  curl -fsSL "$URL/$BIN_NAME" -o "$TMP" || die "下载失败：$URL/$BIN_NAME"
  if [ -n "$BIN_SHA" ]; then
    GOT="$(sha256sum "$TMP" | awk '{print $1}')"
    [ "$GOT" = "$BIN_SHA" ] || die "二进制校验失败：期望 $BIN_SHA，实际 $GOT"
    echo "二进制 SHA-256 校验通过"
  fi
  chmod 755 "$TMP" || die "设置二进制权限失败"

  # 自检放在 enroll 之前：令牌是一次性的，自检失败时令牌还没被消耗，修复后可原样重跑同一条命令。
  echo "运行自检 ..."
  "$TMP" selfcheck --state-dir "$STATE_DIR" || die "自检未通过（令牌尚未使用，未改动已安装的 Agent）；修复上面标 FAIL 的项后重新执行同一条命令"
  mv -f "$TMP" "$BIN_PATH" || die "安装二进制失败"

  # spark-agent 拒绝带 group/other 权限的已有目录：新建与重跑（升级）都强制 0700 root:root。
  install -d -m 0700 -o root -g root "$CONF_DIR" "$STATE_DIR" || die "创建目录失败"
  chmod 700 "$CONF_DIR" "$STATE_DIR" || die "设置目录权限失败"

  echo "接入平台 $PLATFORM ..."
  "$BIN_PATH" enroll --platform "$PLATFORM" --token "$TOKEN" --ca-sha256 "$CA_SHA" --dir "$CONF_DIR" || die "接入失败（令牌可能已被使用）；请在后台重新生成接入命令后再执行"

  cat > "$UNIT" <<'UNITEOF' || die "写入 unit 失败（令牌已使用，请在后台重新生成接入命令后再执行）"
[Unit]
Description=Spark Cloud node agent
After=docker.service network-online.target
Wants=docker.service network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/spark-agent run
Restart=always
RestartSec=3
OOMScoreAdjust=-900
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNITEOF
  chmod 644 "$UNIT"
  systemctl daemon-reload || die "systemctl daemon-reload 失败（令牌已使用，请在后台重新生成接入命令后再执行）"
  systemctl enable spark-agent || die "systemctl enable 失败（令牌已使用，请在后台重新生成接入命令后再执行）"
  systemctl restart spark-agent || die "启动 spark-agent 失败：journalctl -u spark-agent（令牌已使用，请在后台重新生成接入命令后再执行）"
  echo "spark-agent 已安装并启动：systemctl status spark-agent"
}

main "$@"
