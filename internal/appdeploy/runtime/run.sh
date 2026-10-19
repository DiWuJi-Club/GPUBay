#!/bin/sh
# Spark Cloud 应用运行时（平台内置，随部署包下发；用户不要修改，重新部署会覆盖）。
# 用法：sh run.sh deploy|start|stop|status|remove
# 约定：本文件与 app.sh / spec.json / env 同目录；状态写 status.json（原子），输出追加到 log；
#       app.sh 的子命令 install / start / stop，start 必须把服务放后台并把 PID 写进 service.pid。
# 只用 POSIX sh（Ubuntu 的 /bin/sh 是 dash），不要写 bash 语法。
set -u
cd "$(dirname "$0")" || exit 1
DIR=$(pwd)
SEQ_FILE="$DIR/.seq"

load_env() {
  # shellcheck disable=SC1091
  [ -f "$DIR/env" ] && . "$DIR/env"
  : "${SPARK_APP_DIR:=$DIR}" "${SPARK_DATA_DIR:=/root/data}" "${SPARK_PORT:=0}" "${SPARK_SPEC_HASH:=}" "${SPARK_TIMEOUT_S:=3600}"
  : "${SPARK_HEALTH_PATH:=}" "${SPARK_HEALTH_ADDR:=127.0.0.1:$SPARK_PORT}" "${SPARK_MIN_FREE_GIB:=0}"
  export PATH="$DIR:$PATH"
}

now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# 非负十进制整数原样输出，其他（空、带符号、非数字、前导 0）一律 0：status.json 的数字字段不能被文件内容弄坏
num() { case "${1:-}" in ''|*[!0-9]*|0?*) echo 0 ;; *) echo "$1" ;; esac; }

# JSON 字符串转义：反斜杠、双引号；控制字符（含换行、制表、ESC）直接删掉
json_escape() { printf '%s' "$1" | tr -d '\000-\037\177' | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# write_status state phase progress message [service_pid] [exit_code]
write_status() {
  _seq=$(num "$(cat "$SEQ_FILE" 2>/dev/null)")
  _seq=$((_seq + 1))
  echo "$_seq" > "$SEQ_FILE"
  _spid=$(num "${5:-$(cat "$DIR/service.pid" 2>/dev/null)}")
  _pgid=$(num "${SPARK_PGID:-$(cat "$DIR/.pgid" 2>/dev/null)}")
  _tmp="$DIR/status.json.$$"
  printf '{"seq":%s,"state":"%s","phase":"%s","progress":%s,"message":"%s","pid":%s,"service_pid":%s,"spec_hash":"%s","exit_code":%s,"at":"%s"}\n' \
    "$_seq" "$1" "$(json_escape "$2")" "$(num "$3")" "$(json_escape "$4")" "$_pgid" "$_spid" "$(json_escape "$SPARK_SPEC_HASH")" "$(num "${6:-0}")" "$(now_iso)" > "$_tmp"
  mv -f "$_tmp" "$DIR/status.json"
}

# 给 app.sh 用的进度汇报命令：spark_status <phase> <progress> <message>
install_spark_status() {
  cat > "$DIR/spark_status" <<'EOS'
#!/bin/sh
# 由 run.sh 生成：spark_status <phase> <progress> <message>，原子更新同目录 status.json（state 保持 installing）
DIR=$(dirname "$0"); [ -n "${SPARK_APP_DIR:-}" ] && DIR=$SPARK_APP_DIR
exec sh "$DIR/run.sh" _status "${1:-}" "${2:-}" "${3:-}"
EOS
  chmod 755 "$DIR/spark_status"
}

cmd_report() { # cmd_report <phase> <progress> <message>：spark_status 的实现，进度同时记进 log
  _p=$(num "$2"); [ "$_p" -gt 100 ] && _p=100
  write_status installing "$1" "$_p" "$3"
  echo "[spark_status] $1 $_p% $3" >> "$DIR/log"
}

free_gib() {
  if [ -n "${SPARK_TEST_DF_FREE_GIB:-}" ]; then echo "$SPARK_TEST_DF_FREE_GIB"; return; fi
  _kb=$(df -Pk "$SPARK_DATA_DIR" 2>/dev/null | awk 'NR==2 {print $4}')
  echo $(( $(num "$_kb") / 1024 / 1024 ))
}

detach() {
  # 脱离当前 SSH 会话：优先 setsid（util-linux），其次 python3 的 os.setsid，最后普通后台（会随会话结束被 HUP，只在无 setsid 的开发机出现）
  if command -v setsid >/dev/null 2>&1; then
    setsid "$@" </dev/null >>"$DIR/log" 2>&1 &
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' "$@" </dev/null >>"$DIR/log" 2>&1 &
  else
    echo "warn: no setsid/python3, running without session detach" >> "$DIR/log"
    nohup "$@" </dev/null >>"$DIR/log" 2>&1 &
  fi
  echo $!
}

# 本 shell 的进程组号：Linux 读 /proc（精简镜像可能没有 ps），否则 ps
my_pgid() {
  if [ -r "/proc/$$/stat" ]; then
    awk '{print $5}' "/proc/$$/stat"
  else
    ps -o pgid= -p $$ 2>/dev/null | tr -d ' '
  fi
}

# 进程是否还活着；Linux 上僵尸（容器 PID 1 不回收时）算死
pid_alive() {
  [ "$(num "${1:-}")" -gt 1 ] || return 1
  kill -0 "$1" 2>/dev/null || return 1
  if [ -r "/proc/$1/stat" ]; then
    _state=$(sed 's/^.*) //' "/proc/$1/stat" 2>/dev/null | cut -c1)
    [ "$_state" != "Z" ] || return 1
  fi
  return 0
}

# kill_group <pgid> ：TERM，10 秒后 KILL。
# 注意不能写 `kill -TERM -- -<pgid>`：dash 的 kill 不认 `--`（Illegal number），会静默失败；
# `kill -TERM -<pgid>` 在 dash / bash / macOS sh 上都按进程组处理。
kill_group() {
  [ "$(num "${1:-}")" -gt 1 ] || return 0
  kill -TERM "-$1" 2>/dev/null || return 0
  _i=0
  while [ $_i -lt 100 ]; do
    kill -0 "-$1" 2>/dev/null || return 0
    sleep 0.1; _i=$((_i + 1))
  done
  kill -KILL "-$1" 2>/dev/null || true
}

tcp_probe() { # tcp_probe host:port
  _h=${1%:*}; _p=${1##*:}
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket,sys; s=socket.socket(); s.settimeout(2); s.connect((sys.argv[1], int(sys.argv[2]))); s.close()' "$_h" "$_p" 2>/dev/null
  else
    curl -s -o /dev/null --connect-timeout 2 --max-time 3 "telnet://$1" </dev/null; _rc=$?; [ $_rc -eq 0 ] || [ $_rc -eq 28 ]
  fi
}

health_ok() {
  if [ -n "$SPARK_HEALTH_PATH" ]; then
    curl -sf -o /dev/null --connect-timeout 3 --max-time 10 "http://$SPARK_HEALTH_ADDR$SPARK_HEALTH_PATH"
  else
    tcp_probe "$SPARK_HEALTH_ADDR"
  fi
}

cmd_prepare() {
  _free=$(free_gib)
  if [ "$(num "$_free")" -lt "$(num "$SPARK_MIN_FREE_GIB")" ]; then
    write_status failed prepare 0 "disk: 数据盘剩余 ${_free}GiB，需要 ${SPARK_MIN_FREE_GIB}GiB" 0 2
    echo "disk: free ${_free}GiB < ${SPARK_MIN_FREE_GIB}GiB" >&2
    return 2
  fi
  if ! _err=$(sh -n "$DIR/app.sh" 2>&1); then
    write_status failed prepare 0 "app.sh 语法错误: $_err" 0 2
    echo "syntax: $_err" >&2
    return 2
  fi
  install_spark_status
  return 0
}

cmd_deploy() {
  load_env
  cmd_prepare || return $?
  # 旧的部署 / 服务先停掉（幂等：没有也不报错）
  kill_group "$(cat "$DIR/.pgid" 2>/dev/null)"
  rm -f "$DIR/.pgid"
  _n=$(num "$(cat "$DIR/.attempt" 2>/dev/null)"); _n=$((_n + 1)); echo "$_n" > "$DIR/.attempt"
  echo "==== deploy #$_n $(now_iso) ====" >> "$DIR/log"
  write_status installing prepare 0 "starting" 0 0
  _pid=$(detach sh "$DIR/run.sh" _bg deploy)
  echo "started background deploy pid $_pid"
  return 0
}

cmd_bg_deploy() {
  load_env
  # stop / 重新部署按进程组杀：只有本进程是组长（detach 做了 setsid）时才记组号，
  # 否则组里还有调用方（SSH 会话、测试进程），宁可只杀服务进程
  SPARK_PGID=$(num "$(my_pgid)")
  if [ "$SPARK_PGID" != "$$" ]; then
    echo "warn: not a process group leader (pgid $SPARK_PGID, pid $$); stop will only kill service.pid"
    SPARK_PGID=0
  fi
  export SPARK_PGID
  echo "$SPARK_PGID" > "$DIR/.pgid"
  trap 'write_status failed "${PHASE:-prepare}" 0 "interrupted" 0 143; exit 143' TERM INT
  PHASE=prepare
  write_status installing prepare 1 "prepare"
  _installed=$(cat "$DIR/.installed" 2>/dev/null || echo "")
  if [ "$_installed" = "$SPARK_SPEC_HASH" ] && [ -n "$SPARK_SPEC_HASH" ]; then
    echo "skip install: spec_hash $SPARK_SPEC_HASH already installed"
  else
    PHASE=install
    write_status installing install 5 "install"
    rm -f "$DIR/.installed"
    (cd "$DIR" && sh "$DIR/app.sh" install)
    _rc=$?
    if [ "$_rc" -ne 0 ]; then
      # phase 取 app.sh 最后一次 spark_status 汇报的阶段（如 download），失败点更具体
      _ph=$(sed -n 's/.*"phase":"\([A-Za-z0-9_.-]*\)".*/\1/p' "$DIR/status.json" 2>/dev/null)
      write_status failed "${_ph:-install}" 0 "install 失败，退出码 $_rc" 0 "$_rc"
      exit "$_rc"
    fi
    echo "$SPARK_SPEC_HASH" > "$DIR/.installed"
  fi
  PHASE=start
  write_status installing start 80 "start"
  rm -f "$DIR/service.pid"
  (cd "$DIR" && sh "$DIR/app.sh" start)
  _rc=$?
  if [ "$_rc" -ne 0 ]; then
    write_status failed start 80 "start 失败，退出码 $_rc" 0 "$_rc"
    exit "$_rc"
  fi
  _spid=$(num "$(cat "$DIR/service.pid" 2>/dev/null)")
  if [ "$_spid" -le 0 ]; then
    write_status failed start 80 "app.sh start 没有写 service.pid" 0 1
    exit 1
  fi
  PHASE=health
  write_status installing health 90 "health check" "$_spid"
  _deadline=$(( $(date +%s) + $(num "$SPARK_TIMEOUT_S") ))
  while :; do
    if health_ok; then
      write_status running health 100 "healthy" "$_spid"
      exit 0
    fi
    if ! pid_alive "$_spid"; then
      write_status failed health 90 "服务进程已退出" "$_spid" 1
      exit 1
    fi
    if [ "$(date +%s)" -ge "$_deadline" ]; then
      echo "health check timeout after ${SPARK_TIMEOUT_S}s"
      # 先写状态再杀服务：不能 kill 整个进程组（本进程也在组里，trap 会把状态改写成 interrupted）；
      # 同组里残留的子进程（tail 等）由平台随后下发的 run.sh stop 的 kill_group 收尾
      write_status failed health 90 "timeout: 健康检查 ${SPARK_TIMEOUT_S} 秒内未通过" "$_spid" 124
      trap - TERM INT
      kill -TERM "$_spid" 2>/dev/null || true
      sleep 1
      kill -KILL "$_spid" 2>/dev/null || true
      exit 124
    fi
    sleep 5
  done
}

cmd_stop() {
  load_env
  _pgid=$(cat "$DIR/.pgid" 2>/dev/null)
  _spid=$(num "$(cat "$DIR/service.pid" 2>/dev/null)")
  if [ -f "$DIR/app.sh" ] && grep -q '^ *stop)' "$DIR/app.sh" 2>/dev/null; then
    (cd "$DIR" && sh "$DIR/app.sh" stop) >> "$DIR/log" 2>&1 || true
  fi
  kill_group "$_pgid"
  [ "$_spid" -gt 1 ] && kill -KILL "$_spid" 2>/dev/null
  write_status stopped "" 0 "stopped" "$_spid" 0
  return 0
}

log_size() { if [ -f "$DIR/log" ]; then wc -c < "$DIR/log" | tr -d ' '; else echo 0; fi; }

# 第一行 status.json 原文；缺失时输出不带 state 的 {"error":"no status"} 并返回 3，
# 调用方必须保留这个退出码（sh run.sh status; rc=$?; …; exit $rc），不能当成「仍在安装」
cmd_status() {
  if [ ! -f "$DIR/status.json" ]; then
    echo '{"error":"no status"}'; echo "alive=0"; echo "log_size=$(log_size)"
    return 3
  fi
  cat "$DIR/status.json"
  if pid_alive "$(cat "$DIR/service.pid" 2>/dev/null)"; then echo "alive=1"; else echo "alive=0"; fi
  echo "log_size=$(log_size)"
  return 0
}

cmd_remove() {
  cmd_stop >/dev/null 2>&1 || true
  # 只删 <...>/.spark-apps/<slug> 形状的目录：run.sh 被拷到别处误跑时不能 rm -rf 掉别的东西
  if [ "$(basename "$(dirname "$DIR")")" != ".spark-apps" ]; then
    echo "refuse to remove $DIR: not under .spark-apps" >&2
    return 0
  fi
  cd / && rm -rf "$DIR"
  return 0
}

case "${1:-}" in
  deploy|start) cmd_deploy ;;
  _bg) shift; [ "${1:-}" = "deploy" ] && cmd_bg_deploy ;;
  _status) load_env; cmd_report "${2:-}" "${3:-}" "${4:-}" ;;
  stop) cmd_stop ;;
  status) load_env; cmd_status ;;
  remove) load_env; cmd_remove ;;
  *) echo "usage: run.sh deploy|start|stop|status|remove" >&2; exit 64 ;;
esac
