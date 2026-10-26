#!/bin/sh
# Spark Cloud 节点 Agent 安装脚本：下载并校验二进制 -> selfcheck(用临时二进制，先于 enroll) -> 原子替换二进制 -> 目录(0700) -> enroll -> systemd。
# 用法（后台「生成接入命令」给出）：
#   curl -fsSL <URL>/install.sh | sudo sh -s -- --platform host:9443 --token spk1_... --ca-sha256 <fp> --binary-sha256 <sha> --download-url <URL>
# 可重复执行（升级 / 重新接入）。令牌只作为 enroll 的参数传递，脚本不打印它。
# 集群网卡（CX-7 口）：加 --cluster-iface <网卡>（可重复）把它们设为 NetworkManager unmanaged，避免 NM 给它们加地址；
# 已安装的机器可以只带这个参数单独执行：sudo sh install.sh --cluster-iface enp1s0f1np1 --cluster-iface enP2p1s0f1np1
# 每次给出的列表整体替换上次写入的列表；不带 --cluster-iface 的升级不动已有配置。
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
NM_CONF="${SPARK_INSTALL_NM_CONF:-/etc/NetworkManager/conf.d/99-spark-cluster.conf}"
IP_FORWARD="${SPARK_INSTALL_IP_FORWARD:-/proc/sys/net/ipv4/ip_forward}"
NMCLI="${SPARK_INSTALL_NMCLI:-nmcli}"
CLUSTER_IFACES=""

die() { echo "错误: $*" >&2; exit 1; }

# add_cluster_iface 校验网卡名（Linux 网卡名 ≤15 字节；只允许字母数字 . _ -，不能以 - 开头）并去重追加。
add_cluster_iface() {
  case "$1" in
    ""|-*|*[!A-Za-z0-9._-]*) die "--cluster-iface 网卡名不合法: $1" ;;
  esac
  [ "${#1}" -le 15 ] || die "--cluster-iface 网卡名过长: $1"
  case " $CLUSTER_IFACES " in
    *" $1 "*) ;;
    *) CLUSTER_IFACES="${CLUSTER_IFACES:+$CLUSTER_IFACES }$1" ;;
  esac
}

# write_nm_conf 把集群网卡写成 NetworkManager unmanaged 并 reload。用 unmanaged-devices+=（追加）而不是 =：
# Ubuntu 的 /usr/lib/NetworkManager/conf.d/10-globally-managed-devices.conf 可能已设 unmanaged-devices=*,except:type:wifi…，
# 用 = 会把它整个覆盖掉，让 NM 开始接管原本不归它管的有线网卡（包括管理网卡）。
write_nm_conf() {
  [ -n "$CLUSTER_IFACES" ] || return 0
  LIST=""
  for i in $CLUSTER_IFACES; do LIST="${LIST:+$LIST;}interface-name:$i"; done
  mkdir -p "$(dirname "$NM_CONF")" || die "创建 $(dirname "$NM_CONF") 失败"
  {
    echo "# 由 spark-agent install.sh --cluster-iface 生成：集群网卡（CX-7 口）不归 NetworkManager 管理，避免 NM 给它们加地址。"
    echo "[keyfile]"
    echo "unmanaged-devices+=$LIST"
  } > "$NM_CONF.tmp" || die "写入 $NM_CONF 失败"
  chmod 644 "$NM_CONF.tmp" && mv -f "$NM_CONF.tmp" "$NM_CONF" || die "写入 $NM_CONF 失败"
  echo "已写入 ${NM_CONF}（集群网卡: ${CLUSTER_IFACES}）"
  if command -v "$NMCLI" >/dev/null 2>&1; then
    "$NMCLI" general reload || echo "警告: nmcli general reload 失败；NetworkManager 运行时请手动执行 nmcli general reload" >&2
  else
    echo "提示: 未找到 nmcli（未安装 NetworkManager），跳过 reload"
  fi
}

# cluster_hints 安装后的集群网络提示：CX-7 口不要配 IP；lan 集群（routed）需要 ip_forward=1。
cluster_hints() {
  echo "提示: 集群用的 CX-7 口不要配置 IP 地址（Agent 会持续清除其地址），可用 --cluster-iface 设为 NetworkManager unmanaged"
  IPF="$(cat "$IP_FORWARD" 2>/dev/null || echo "?")"
  [ "$IPF" = "1" ] || echo "提示: ${IP_FORWARD} 当前为 ${IPF}；lan 集群需要 1：sysctl -w net.ipv4.ip_forward=1（Docker 启动时通常会打开）"
}

main() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --platform|--token|--ca-sha256|--binary-sha256|--download-url|--cluster-iface)
        [ $# -ge 2 ] || die "参数 $1 缺少取值" ;;
    esac
    case "$1" in
      --platform) PLATFORM="$2"; shift 2 ;;
      --token) TOKEN="$2"; shift 2 ;;
      --ca-sha256) CA_SHA="$2"; shift 2 ;;
      --binary-sha256) BIN_SHA="$2"; shift 2 ;;
      --download-url) URL="$2"; shift 2 ;;
      --cluster-iface) add_cluster_iface "$2"; shift 2 ;;
      *) die "未知参数: $1" ;;
    esac
  done
  # 只给 --cluster-iface：只写 NetworkManager 配置（已安装的机器补设集群网卡），不下载、不接入。
  if [ -n "$CLUSTER_IFACES" ] && [ -z "$PLATFORM$TOKEN$CA_SHA" ]; then
    [ "$(id -u)" = "0" ] || die "请用 root 执行（sudo）"
    write_nm_conf
    cluster_hints
    return 0
  fi
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

  # NM 配置在自检之前写：自检据此不再对这些口报「受 NetworkManager 管理」；它与已安装的 Agent 无关，重跑幂等。
  write_nm_conf

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
# network-online 在未启用 *-wait-online 的机器上不保证已有地址，恢复启动失败由 Agent 重试兜底
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
  cluster_hints
}

main "$@"
