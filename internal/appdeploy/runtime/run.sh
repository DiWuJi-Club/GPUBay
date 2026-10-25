#!/bin/sh
# Spark Cloud 应用运行时（平台内置，随部署包下发；用户不要修改，重新部署会覆盖）。
# 用法：sh run.sh deploy|start|stop|status|remove
# 约定：本文件与 app.sh / spec.json / env（/ image.env）同目录；状态写 status.json（原子），输出追加到 log；
#       app.sh 的子命令 install / start / stop，start 必须把服务放后台并把 PID 写进 service.pid。
# 只用 POSIX sh（Ubuntu 的 /bin/sh 是 dash），不要写 bash 语法；日志行用 printf（dash 的 echo 会解释反斜杠）。
set -u
cd "$(dirname "$0")" || exit 1
DIR=$(pwd)
SEQ_FILE="$DIR/.seq"
# 时间预算（Runner 对 stop / deploy 的 exec 上限都是 ExecStatusTimeout = 20 秒）：
#   stop  ：app.sh stop 5 秒 + 进程组 TERM 后等 10 秒 + 服务进程 TERM 后等 2 秒 = 最坏 17 秒
#   deploy：先停旧的 app.sh stop 2 秒 + 进程组 4 秒 + 服务 1 秒，再等新进程组出现 ≤ 2 秒，
#           加 prepare（df、sh -n）不到 1 秒 = 最坏约 10 秒（要求 ≤ 12 秒）
STOP_APP_S=5 STOP_GROUP_DS=100 STOP_PID_DS=20      # stop / remove（DS = 0.1 秒）
DEPLOY_APP_S=2 DEPLOY_GROUP_DS=40 DEPLOY_PID_DS=10 # deploy 前停旧的

load_env() {
  # image.env = 镜像的 Docker ENV（CUDA_HOME、LD_LIBRARY_PATH、PATH …；平台经 SSH 执行本脚本，会话里没有这些），平台部署前从
  # 容器读出、过滤后随包下发；先加载它，再加载 env（平台保留变量与用户变量），同名以 env 为准
  # shellcheck disable=SC1091
  [ -f "$DIR/image.env" ] && . "$DIR/image.env"
  # shellcheck disable=SC1091
  [ -f "$DIR/env" ] && . "$DIR/env"
  : "${SPARK_APP_DIR:=$DIR}" "${SPARK_DATA_DIR:=/root/data}" "${SPARK_PORT:=0}" "${SPARK_SPEC_HASH:=}" "${SPARK_TIMEOUT_S:=3600}"
  : "${SPARK_HEALTH_PATH:=}" "${SPARK_HEALTH_ADDR:=127.0.0.1:$SPARK_PORT}" "${SPARK_MIN_FREE_GIB:=0}"
  export PATH="$DIR:$PATH"
}

now_iso() { date -u +%Y-%m-%dT%H:%M:%SZ; }

logln() { printf '%s\n' "$*" >> "$DIR/log"; }

# 非负十进制整数原样输出，其他（空、带符号、非数字、前导 0）一律 0：status.json 的数字字段不能被文件内容弄坏
num() { case "${1:-}" in ''|*[!0-9]*|0?*) echo 0 ;; *) echo "$1" ;; esac; }

# JSON 字符串转义：控制字符（含换行、制表、ESC）删掉，截到 1024 字符，再转义反斜杠、双引号
json_escape() { printf '%s' "$1" | tr -d '\000-\037\177' | cut -c1-1024 | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }

# 原子写小文件：临时文件 + mv
put() { printf '%s\n' "$2" > "$1.$$" && mv -f "$1.$$" "$1"; }

# write_status state phase progress message [service_pid] [exit_code]
write_status() {
  _seq=$(num "$(cat "$SEQ_FILE" 2>/dev/null)")
  _seq=$((_seq + 1))
  put "$SEQ_FILE" "$_seq"
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
  logln "[spark_status] $1 $_p% $3"
}

free_gib() {
  if [ -n "${SPARK_TEST_DF_FREE_GIB:-}" ]; then echo "$SPARK_TEST_DF_FREE_GIB"; return; fi
  _kb=$(df -Pk "$SPARK_DATA_DIR" 2>/dev/null | awk 'NR==2 {print $4}')
  echo $(( $(num "$_kb") / 1024 / 1024 ))
}

# install 跳过标记的键：spec_hash + env 文件校验和 + workflows/manifest.json 校验和（换 MODEL_ID 等变量、改工作流集合都要重跑
# install；install 自身幂等，模型缓存保留）。没有 manifest 时第三段为空串（键以空格结尾）。
install_key() {
  _mk=""
  [ -f "$DIR/workflows/manifest.json" ] && _mk=$(cksum < "$DIR/workflows/manifest.json" 2>/dev/null | awk '{print $1 "-" $2}')
  printf '%s %s %s' "$SPARK_SPEC_HASH" "$(cksum < "$DIR/env" 2>/dev/null | awk '{print $1 "-" $2}')" "$_mk"
}

# 去掉尾部空格：旧版两段式 .installed（「hash env」）与没有 manifest 的新键（「hash env 」）视为相同，升级后不重跑 install
rtrim() { printf '%s' "$1" | sed 's/ *$//'; }

# 脱离当前 SSH 会话后台运行。输出「<pid> <1|0>」：1 = 已成为新会话 / 进程组的组长（pgid = pid）
detach() {
  # 优先 setsid（util-linux），其次 python3 的 os.setsid，最后普通后台（会随会话结束被 HUP，只在无 setsid 的开发机出现）
  if command -v setsid >/dev/null 2>&1; then
    setsid "$@" </dev/null >>"$DIR/log" 2>&1 &
    echo "$! 1"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1], sys.argv[1:])' "$@" </dev/null >>"$DIR/log" 2>&1 &
    echo "$! 1"
  else
    logln "warn: no setsid/python3, running without session detach"
    nohup "$@" </dev/null >>"$DIR/log" 2>&1 &
    echo "$! 0"
  fi
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

# kill_group <pgid> [等待，0.1 秒为单位，默认 100]：TERM，等不到全部退出就 KILL。
# 目标是本脚本自己所在的进程组（.pgid 陈旧 / pid 被复用）时拒绝，否则会连 SSH 会话一起杀掉。
# 注意不能写 `kill -TERM -- -<pgid>`：dash 的 kill 不认 `--`（Illegal number），会静默失败；
# `kill -TERM -<pgid>` 在 dash / bash / macOS sh 上都按进程组处理。
kill_group() {
  [ "$(num "${1:-}")" -gt 1 ] || return 0
  if [ "$1" = "$(num "$(my_pgid)")" ]; then
    logln "warn: refuse to kill own process group $1 (stale .pgid)"
    return 0
  fi
  kill -TERM "-$1" 2>/dev/null || return 0
  _i=0
  while [ $_i -lt "${2:-100}" ]; do
    kill -0 "-$1" 2>/dev/null || return 0
    sleep 0.1; _i=$((_i + 1))
  done
  kill -KILL "-$1" 2>/dev/null || true
}

# kill_pid <pid> [等待，0.1 秒为单位，默认 20]：TERM，等不到退出就 KILL（服务自己 setsid 出了进程组时 kill_group 够不着）
kill_pid() {
  pid_alive "${1:-}" || return 0
  kill -TERM "$1" 2>/dev/null
  _i=0
  while [ $_i -lt "${2:-20}" ]; do
    pid_alive "$1" || return 0
    sleep 0.1; _i=$((_i + 1))
  done
  kill -KILL "$1" 2>/dev/null || true
}

# app.sh 里有 stop 分支吗（容忍制表符缩进、`(stop)`、`restart|stop)` 这类写法）
has_stop() {
  grep -Eq '^[[:space:]]*\(?([^)#]*\|)?[[:space:]]*stop[[:space:]]*(\|[^)]*)?\)' "$DIR/app.sh" 2>/dev/null
}

# app_stop <时限秒>：限时执行 app.sh stop，输出进日志
app_stop() {
  _lim=$1
  [ -f "$DIR/app.sh" ] && has_stop || return 0
  if command -v timeout >/dev/null 2>&1; then
    (cd "$DIR" && timeout -s KILL "$_lim" sh "$DIR/app.sh" stop) </dev/null >>"$DIR/log" 2>&1
  else
    # 没有 timeout（macOS）：后台跑 + 看门狗。整段放子 shell 并丢弃 stderr，免得 bash 打印 "Terminated" 作业通知
    (
      (cd "$DIR" && exec sh "$DIR/app.sh" stop) </dev/null >>"$DIR/log" 2>&1 &
      _ap=$!
      (sleep "$_lim"; kill -KILL "$_ap") </dev/null >/dev/null 2>&1 &
      _wd=$!
      wait "$_ap"
      kill "$_wd"
    ) 2>/dev/null
  fi
  return 0
}

# stop_procs <always|tracked> <app.sh stop 秒> <进程组等待> <服务等待>：
# 停掉本应用的全部进程（app.sh stop → 进程组 → 服务进程），清掉 pid 文件，免得之后误杀被复用的 pid。
# tracked = 只有记录了 pgid / service.pid 时才跑 app.sh stop（首次部署不跑）
stop_procs() {
  _pg=$(num "$(cat "$DIR/.pgid" 2>/dev/null)")
  _sp=$(num "$(cat "$DIR/service.pid" 2>/dev/null)")
  [ "$1" = always ] || [ "$_pg" -gt 1 ] || [ "$_sp" -gt 1 ] && app_stop "$2"
  kill_group "$_pg" "$3"
  kill_pid "$_sp" "$4"
  rm -f "$DIR/.pgid" "$DIR/service.pid"
}

tcp_probe() { # tcp_probe host:port
  _h=${1%:*}; _p=${1##*:}
  if [ "${SPARK_TEST_TCP_PROBE:-}" != curl ] && command -v python3 >/dev/null 2>&1; then
    python3 -c 'import socket,sys; s=socket.socket(); s.settimeout(2); s.connect((sys.argv[1], int(sys.argv[2]))); s.close()' "$_h" "$_p" 2>/dev/null
  else
    # telnet:// 连上后会一直等数据直到 --max-time（rc 28），连接超时也是 rc 28：用 time_connect 区分是否真的连上了
    _tc=$(curl -s -o /dev/null -w '%{time_connect}' --connect-timeout 2 --max-time 3 "telnet://$1" </dev/null)
    _rc=$?
    [ $_rc -eq 0 ] && return 0
    [ $_rc -eq 28 ] && awk -v t="${_tc:-0}" 'BEGIN { exit !(t + 0 > 0) }'
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
    printf 'disk: free %sGiB < %sGiB\n' "$_free" "$SPARK_MIN_FREE_GIB" >&2
    return 2
  fi
  if ! _err=$(sh -n "$DIR/app.sh" 2>&1); then
    write_status failed prepare 0 "app.sh 语法错误: $_err" 0 2
    printf 'syntax: %s\n' "$_err" >&2
    return 2
  fi
  install_spark_status
  return 0
}

# deploy 前台部分互斥（stop_procs → prepare → detach）：Runner 的 deploy exec 超时（20 秒）后命令可能还在容器里跑，
# 下一轮会再 deploy；两个并发的 deploy 会各自 detach 一个 _bg。锁是符号链接（ln -s 原子且自带内容「pid 秒」），
# 持有者已死或超过 DEPLOY_LOCK_S 秒视为陈旧（被 SIGKILL 的 deploy 留下的）。
# 锁确实被别人持有：stderr「busy: …」，退出 75；建不了锁（只读、权限、同名普通文件）是真错误：退出 1。
DEPLOY_LOCK_S=120
DEPLOY_BUSY=75
deploy_lock() {
  _lk="$DIR/.deploy.lock"
  _own="$$ $(date +%s)"
  ln -s "$_own" "$_lk" 2>/dev/null && return 0
  if [ ! -L "$_lk" ]; then
    printf 'deploy lock: cannot create %s\n' "$_lk" >&2
    return 1
  fi
  _cur=$(readlink "$_lk" 2>/dev/null)
  _lp=$(num "${_cur%% *}"); _lt=$(num "${_cur##* }")
  if pid_alive "$_lp" && [ $(( $(date +%s) - _lt )) -lt "$DEPLOY_LOCK_S" ]; then
    printf 'busy: deploy pid %s still running\n' "$_lp" >&2
    return "$DEPLOY_BUSY"
  fi
  # 打破陈旧锁要原子：先 rename 到本进程独有的名字，再核对拿到的正是刚才判定为陈旧的那个；
  # 若别人抢先换上了新锁（被我们误挪走），原样放回（ln -s 不覆盖：此时又有第三者建了锁就算它的）
  _stale="$_lk.stale.$$"
  if mv -f "$_lk" "$_stale" 2>/dev/null; then
    _got=$(readlink "$_stale" 2>/dev/null)
    if [ "$_got" != "$_cur" ]; then
      ln -s "$_got" "$_lk" 2>/dev/null
      rm -f "$_stale"
      printf 'busy: deploy lock taken over\n' >&2
      return "$DEPLOY_BUSY"
    fi
    rm -f "$_stale"
  fi
  ln -s "$_own" "$_lk" 2>/dev/null && return 0
  if [ -L "$_lk" ]; then
    printf 'busy: deploy lock contended\n' >&2
    return "$DEPLOY_BUSY"
  fi
  printf 'deploy lock: cannot create %s\n' "$_lk" >&2
  return 1
}
deploy_unlock() { [ "$(readlink "$DIR/.deploy.lock" 2>/dev/null)" = "$_own" ] && rm -f "$DIR/.deploy.lock"; return 0; }

cmd_deploy() {
  load_env
  # 每次 deploy 换新日志（旧的留一份 log.1）：平台按当前 secret 值脱敏日志尾部，换过 secret 后旧日志里的旧值认不出来
  [ -f "$DIR/log" ] && mv -f "$DIR/log" "$DIR/log.1"
  # 旧的部署 / 服务先停掉（幂等：没有也不报错）。放在 prepare 之前：prepare 失败时旧服务也已停，状态 failed 与实际一致
  stop_procs tracked "$DEPLOY_APP_S" "$DEPLOY_GROUP_DS" "$DEPLOY_PID_DS"
  cmd_prepare || return $?
  _n=$(num "$(cat "$DIR/.attempt" 2>/dev/null)"); _n=$((_n + 1)); put "$DIR/.attempt" "$_n"
  logln "==== deploy #$_n $(now_iso) ===="
  write_status installing prepare 0 "starting" 0 0
  _out=$(detach sh "$DIR/run.sh" _bg deploy)
  _pid=${_out% *}
  if [ "${_out#* }" = 1 ]; then
    # 立刻记下组号：deploy 返回后马上 stop / remove 也能杀到 _bg（它自己写 .pgid 之前）。
    # 等到 setsid 真正生效（进程组出现）再返回，否则那一瞬间的 kill -<pgid> 会落空
    put "$DIR/.pgid" "$_pid"
    _i=0
    while [ $_i -lt 100 ] && kill -0 "$_pid" 2>/dev/null && ! kill -0 "-$_pid" 2>/dev/null; do
      sleep 0.02; _i=$((_i + 1))
    done
  fi
  printf 'started background deploy pid %s\n' "$_pid"
  return 0
}

# _bg 的失败出口：先写 failed 状态，再杀服务进程（它可能自己 setsid 离开了本组）和整个进程组
# （孙进程、本进程自己），pid 文件在杀完之后才清除，中途 stop 仍能按文件找到它们
fail_exit() { # fail_exit <phase> <progress> <message> <service_pid> <exit_code>
  write_status failed "$1" "$2" "$3" "$4" "$5"
  trap '' TERM INT
  _fs=$(num "$4"); [ "$_fs" -gt 1 ] || _fs=$(num "$(cat "$DIR/service.pid" 2>/dev/null)")
  if [ "$(num "$SPARK_PGID")" -gt 1 ]; then
    kill -TERM "-$SPARK_PGID" 2>/dev/null
    sleep 1
    kill_pid "$_fs"
    rm -f "$DIR/.pgid" "$DIR/service.pid"
    kill -KILL "-$SPARK_PGID" 2>/dev/null # 本进程也在组里，到这里为止
  else
    kill_pid "$_fs"
    rm -f "$DIR/.pgid" "$DIR/service.pid"
  fi
  exit "$5"
}

cmd_bg_deploy() {
  load_env
  # stop / 重新部署按进程组杀：只有本进程是组长（detach 做了 setsid）时才记组号，
  # 否则组里还有调用方（SSH 会话、测试进程），宁可只杀服务进程
  SPARK_PGID=$(num "$(my_pgid)")
  if [ "$SPARK_PGID" != "$$" ]; then
    printf 'warn: not a process group leader (pgid %s, pid %s); stop will only kill service.pid\n' "$SPARK_PGID" "$$"
    SPARK_PGID=0
  fi
  export SPARK_PGID
  put "$DIR/.pgid" "$SPARK_PGID"
  trap 'write_status failed "${PHASE:-prepare}" 0 "interrupted" 0 143; exit 143' TERM INT
  PHASE=prepare
  write_status installing prepare 1 "prepare"
  _key=$(install_key)
  _installed=$(cat "$DIR/.installed" 2>/dev/null || echo "")
  if [ "$(rtrim "$_installed")" = "$(rtrim "$_key")" ] && [ -n "$SPARK_SPEC_HASH" ]; then
    printf 'skip install: spec_hash %s, env and workflows unchanged\n' "$SPARK_SPEC_HASH"
  else
    PHASE=install
    write_status installing install 5 "install"
    rm -f "$DIR/.installed"
    (cd "$DIR" && sh "$DIR/app.sh" install)
    _rc=$?
    if [ "$_rc" -ne 0 ]; then
      # phase 取 app.sh 最后一次 spark_status 汇报的阶段（如 download），失败点更具体
      _ph=$(sed -n 's/.*"phase":"\([A-Za-z0-9_.-]*\)".*/\1/p' "$DIR/status.json" 2>/dev/null)
      # app.sh 磁盘预检失败时最后一次汇报的消息以 disk: 开头（计划 11 D7）：沿用它，平台据前缀映射为错误码 disk
      # 取 "message":"…","pid": 之间（write_status 的字段顺序：message 后紧跟 pid；消息里的引号已转义成 \"，不会提前匹配），
      # 再还原 json_escape 的转义（先 \" → "，再 \\ → \），fail_exit 写回时重新转义
      _msg=$(sed -n 's/.*"message":"\(disk:.*\)","pid":.*/\1/p' "$DIR/status.json" 2>/dev/null | sed -e 's/\\"/"/g' -e 's/\\\\/\\/g')
      fail_exit "${_ph:-install}" 0 "${_msg:-install 失败，退出码 $_rc}" 0 "$_rc"
    fi
    put "$DIR/.installed" "$_key"
  fi
  PHASE=start
  write_status installing start 80 "start"
  rm -f "$DIR/service.pid"
  (cd "$DIR" && sh "$DIR/app.sh" start)
  _rc=$?
  [ "$_rc" -eq 0 ] || fail_exit start 80 "start 失败，退出码 $_rc" 0 "$_rc"
  _spid=$(num "$(cat "$DIR/service.pid" 2>/dev/null)")
  [ "$_spid" -gt 0 ] || fail_exit start 80 "app.sh start 没有写 service.pid" 0 1
  PHASE=health
  write_status installing health 90 "health check" "$_spid"
  _deadline=$(( $(date +%s) + $(num "$SPARK_TIMEOUT_S") ))
  while :; do
    if health_ok; then
      write_status running health 100 "healthy" "$_spid"
      exit 0
    fi
    pid_alive "$_spid" || fail_exit health 90 "服务进程已退出" "$_spid" 1
    if [ "$(date +%s)" -ge "$_deadline" ]; then
      printf 'health check timeout after %ss\n' "$SPARK_TIMEOUT_S"
      fail_exit health 90 "timeout: 健康检查 ${SPARK_TIMEOUT_S} 秒内未通过" "$_spid" 124
    fi
    sleep 5
  done
}

cmd_stop() {
  load_env
  _sp0=$(num "$(cat "$DIR/service.pid" 2>/dev/null)")
  stop_procs always "$STOP_APP_S" "$STOP_GROUP_DS" "$STOP_PID_DS"
  write_status stopped "" 0 "stopped" "$_sp0" 0
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
  # 只删 <...>/.spark-apps/<slug> 形状的目录：run.sh 被拷到别处误跑时不能 rm -rf 掉别的东西
  if [ "$(basename "$(dirname "$DIR")")" != ".spark-apps" ]; then
    printf 'refuse to remove %s: not under .spark-apps\n' "$DIR" >&2
    return 0
  fi
  cmd_stop >/dev/null 2>&1 || true
  cd / && rm -rf "$DIR"
  return 0
}

case "${1:-}" in
  deploy|start) deploy_lock || exit $?; cmd_deploy; _rc=$?; deploy_unlock; exit $_rc ;;
  _bg) shift; [ "${1:-}" = "deploy" ] && cmd_bg_deploy ;;
  _status) load_env; cmd_report "${2:-}" "${3:-}" "${4:-}" ;;
  stop) cmd_stop ;;
  status) load_env; cmd_status ;;
  remove) load_env; cmd_remove ;;
  *) echo "usage: run.sh deploy|start|stop|status|remove" >&2; exit 64 ;;
esac
