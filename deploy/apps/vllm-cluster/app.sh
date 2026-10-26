#!/bin/sh
# vLLM 多机推理（集群应用，计划 13）：在 2～4 台集群上用 vLLM + Ray 部署一个大模型。
# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用，env 里的变量已导出，spark_status 在 PATH 上。
# 只能作为集群部署：平台给每个成员注入 SPARK_CLUSTER_ROLE（rank0 = head，其余 = worker），集群拓扑读 /.spark/cluster.env。
# install = 每个成员都下载同一份模型；start = head 跑 Ray head + vllm serve，worker 加入 Ray 集群并起回环健康服务。
set -eu

# 平台没配置 HF 镜像时 HF_ENDPOINT 是空串，huggingface_hub 会拼出无效地址：空就去掉，用默认值
[ -n "${HF_ENDPOINT:-}" ] || unset HF_ENDPOINT
# 下载工具的进度条（\r 刷屏）不进日志，进度由 spark_status 汇报
export TQDM_DISABLE=1 HF_HUB_DISABLE_PROGRESS_BARS=1

# 模型大小（GB）只用于进度估算：模型任意，没有大小常量表，按 MODEL_SIZE_GB（缺省 60）
size_gb() {
  echo "${MODEL_SIZE_GB:-60}"
}

model_dir() {
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) echo "$SPARK_MODELS_DIR/hf/$MODEL_ID" ;;
    *) echo "$SPARK_MODELS_DIR/modelscope/$MODEL_ID" ;;
  esac
}

# 后台每 10 秒按 du 汇报下载百分比（5～75）
progress_loop() {
  _dir=$1; _total_kb=$(( $(size_gb) * 1024 * 1024 ))
  while :; do
    sleep 10
    _kb=$(du -sk "$_dir" 2>/dev/null | awk '{print $1}')
    _pct=$(( 5 + ${_kb:-0} * 70 / _total_kb ))
    [ "$_pct" -gt 75 ] && _pct=75
    spark_status download "$_pct" "下载模型 ${MODEL_ID}：$(( ${_kb:-0} / 1024 / 1024 ))GB / 约 $(size_gb)GB"
  done
}

download() {
  _dir=$(model_dir)
  mkdir -p "$_dir"
  spark_status download 5 "开始下载 ${MODEL_ID}（约 $(size_gb)GB）"
  progress_loop "$_dir" &
  _pl=$!
  # 下载失败（set -e 退出）时也要停掉进度循环，否则它会在 run.sh 写 failed 之后继续写 installing
  trap 'kill "$_pl" 2>/dev/null || true' EXIT
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror)
      # HF_ENDPOINT 由平台注入（hf-mirror）；hf 命令随 huggingface_hub 安装
      hf download "$MODEL_ID" --local-dir "$_dir" ;;
    *)
      # 已下载完整的文件会跳过（重建节点 / 换回旧模型秒过）
      modelscope download "$MODEL_ID" --local-dir "$_dir" ;;
  esac
  kill "$_pl" 2>/dev/null || true
  trap - EXIT
  spark_status install 78 "模型就绪"
}

# ---- spark-cluster-ray begin ----
# 多机（计划 13）。SPARK_CLUSTER_ROLE 只对集群部署注入：head（rank0）/ worker。集群拓扑读 Agent 维护的 /.spark/cluster.env：
# 每行都是 export，VLLM_HOST_IP / NCCL_SOCKET_IFNAME / GLOO_SOCKET_IFNAME（集群网卡 spc0）随之导出给 ray start、vllm serve 及其子进程。
# vLLM 0.10.1 多机只能走 Ray：head `ray start --head` → 等 N 个带 GPU 的节点 → vllm serve --distributed-executor-backend ray；
# worker `ray start --address=<head>:6379 --block`（断开后每 10 秒重连，head 重建后自动加入）+ 回环健康服务。
# 本段在 deploy/apps/vllm-node 与 deploy/apps/vllm-cluster 的 app.sh 里逐字相同（apps_dir_test 断言），改一处要同步另一处。
CLUSTER_RAY_PORT=6379
# GB10 是统一内存：Ray 默认按 30% 内存预留对象存储会挤占显存，固定 2GB
CLUSTER_OBJECT_STORE=2000000000

# cluster_fail <消息>：参数 / 环境问题（重试无用）——汇报后以退出码 2 结束
cluster_fail() {
  spark_status install 0 "$1"
  printf 'cluster: %s\n' "$1" >&2
  exit 2
}

# cluster_load：读集群信息、校验角色与并行参数，设置 CLUSTER_TP / CLUSTER_PP（默认 TP = 台数、PP = 1）
cluster_load() {
  case "${SPARK_CLUSTER_ROLE:-}" in
    head|worker) ;;
    *) cluster_fail "只能在集群上部署（请在集群页部署）" ;;
  esac
  if [ -n "${SPARK_TEST_CLUSTER_ENV:-}" ]; then # 测试钩子：真机上读 /.spark/cluster.env
    [ -f "$SPARK_TEST_CLUSTER_ENV" ] || cluster_fail "集群信息缺失：$SPARK_TEST_CLUSTER_ENV 不存在"
    . "$SPARK_TEST_CLUSTER_ENV"
  else
    [ -f /.spark/cluster.env ] || cluster_fail "集群信息缺失：/.spark/cluster.env 不存在"
    . /.spark/cluster.env
  fi
  case "${SPARK_CLUSTER_SIZE:-}" in
    ''|*[!0-9]*) cluster_fail "集群信息缺失：SPARK_CLUSTER_SIZE 无效" ;;
  esac
  if [ -z "${SPARK_CLUSTER_IP:-}" ] || [ -z "${SPARK_CLUSTER_HEAD_IP:-}" ]; then
    cluster_fail "集群信息缺失：没有集群 IP"
  fi
  CLUSTER_TP=${TENSOR_PARALLEL_SIZE:-$SPARK_CLUSTER_SIZE}
  CLUSTER_PP=${PIPELINE_PARALLEL_SIZE:-1}
  if [ $((CLUSTER_TP * CLUSTER_PP)) -ne "$SPARK_CLUSTER_SIZE" ]; then
    cluster_fail "TP×PP 必须等于集群台数（TP ${CLUSTER_TP} × PP ${CLUSTER_PP}，集群 ${SPARK_CLUSTER_SIZE} 台）"
  fi
  # vLLM 0.10 的 Ray 流水线并行走 Ray Compiled Graph，需要 ray[cgraph] 的 cupy（spark/llm-serving:25.09 没有）
  if [ "$CLUSTER_PP" -gt 1 ] && ! python3 -c 'import cupy, ray.dag' >/dev/null 2>&1; then
    cluster_fail "流水线并行需要镜像内 cupy，请改用 TP = 台数"
  fi
}

# cluster_probe <host> <port>：TCP 能否连上（镜像里没有 ping；用 python3，2 秒超时）
cluster_probe() {
  python3 -c 'import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), 2).close()' "$1" "$2" >/dev/null 2>&1
}

# wait_nodes <N> <超时秒>：Ray 里存活且带 GPU 的节点数达到 N
wait_nodes() {
  python3 - "$1" "$2" <<'PY'
import sys, time, ray
n, deadline = int(sys.argv[1]), time.time() + int(sys.argv[2])
ray.init(address="auto", logging_level="ERROR")
last = -1
while time.time() < deadline:
    alive = [x for x in ray.nodes() if x.get("Alive") and x.get("Resources", {}).get("GPU", 0) >= 1]
    if len(alive) != last:
        last = len(alive)
        print("ray nodes with GPU: %d/%d %s" % (last, n, sorted(x.get("NodeManagerAddress", "") for x in alive)), flush=True)
    if len(alive) >= n: sys.exit(0)
    time.sleep(5)
sys.exit(1)
PY
}

# cluster_start_head <vllm serve 命令与参数…>（不含并行参数）：ray head 前台起好，后台等齐节点后 exec vllm serve；
# 后台进程 PID 放进 CLUSTER_SERVICE_PID（exec 之后就是 vllm 本身），输出追加到 $SPARK_APP_DIR/vllm.log
cluster_start_head() {
  ray stop --force >/dev/null 2>&1 || true
  ray start --head --node-ip-address="$SPARK_CLUSTER_IP" --port="$CLUSTER_RAY_PORT" --num-gpus=1 \
    --object-store-memory="$CLUSTER_OBJECT_STORE" --dashboard-host=127.0.0.1 --disable-usage-stats
  set -- "$@" --distributed-executor-backend ray --tensor-parallel-size "$CLUSTER_TP" --pipeline-parallel-size "$CLUSTER_PP"
  (
    printf 'waiting for %s ray nodes (timeout %ss)\n' "$SPARK_CLUSTER_SIZE" "${SPARK_TIMEOUT_S:-3600}"
    if ! wait_nodes "$SPARK_CLUSTER_SIZE" "${SPARK_TIMEOUT_S:-3600}"; then
      spark_status health 90 "worker 未加入：${SPARK_TIMEOUT_S:-3600} 秒内 Ray 集群不足 $SPARK_CLUSTER_SIZE 个 GPU 节点"
      exit 1
    fi
    # 容器有 eth0 与 spc0 两块网卡：显式指定 Ray 地址，driver 才不会自动选到 eth0 的 10.200.0.x
    export RAY_ADDRESS="$SPARK_CLUSTER_HEAD_IP:$CLUSTER_RAY_PORT"
    exec "$@"
  ) </dev/null >>"$SPARK_APP_DIR/vllm.log" 2>&1 &
  CLUSTER_SERVICE_PID=$!
}

# cluster_start_worker：回环健康服务（GET /health：raylet 进程在 → 200，否则 503）+ 后台循环 ray start --block；
# 循环进程的 PID 放进 CLUSTER_SERVICE_PID
cluster_start_worker() {
  ray stop --force >/dev/null 2>&1 || true
  cat > "$SPARK_APP_DIR/health.py" <<'PY'
import http.server, os, sys

def raylet_alive():
    try:
        pids = [p for p in os.listdir("/proc") if p.isdigit()]
    except OSError:
        return False
    for p in pids:
        try:
            with open("/proc/%s/comm" % p) as f:
                if f.read().strip() == "raylet":
                    return True
        except OSError:
            pass
    return False

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.split("?", 1)[0] != "/health":
            code, body = 404, b"not found\n"
        elif raylet_alive():
            code, body = 200, b"ok\n"
        else:
            code, body = 503, b"raylet not running\n"
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

host, port = sys.argv[1].rsplit(":", 1)
http.server.ThreadingHTTPServer((host, int(port)), Handler).serve_forever()
PY
  python3 -I "$SPARK_APP_DIR/health.py" "127.0.0.1:$SPARK_PORT" </dev/null >>"$SPARK_APP_DIR/health.log" 2>&1 &
  echo $! > "$SPARK_APP_DIR/health.pid"
  (
    while :; do
      cluster_probe "$SPARK_CLUSTER_HEAD_IP" "$CLUSTER_RAY_PORT" || printf 'head %s:%s not reachable yet\n' "$SPARK_CLUSTER_HEAD_IP" "$CLUSTER_RAY_PORT"
      ray start --address="$SPARK_CLUSTER_HEAD_IP:$CLUSTER_RAY_PORT" --node-ip-address="$SPARK_CLUSTER_IP" --num-gpus=1 \
        --object-store-memory="$CLUSTER_OBJECT_STORE" --disable-usage-stats --block || true
      printf 'ray worker exited, reconnecting in 10s\n'
      ray stop --force >/dev/null 2>&1 || true
      sleep 10
    done
  ) </dev/null &
  CLUSTER_SERVICE_PID=$!
}

# cluster_stop：停健康服务 / 日志尾巴，再 ray stop --force（ray 的守护进程不一定在 run.sh 杀的进程组里）
cluster_stop() {
  for _pf in health.pid tail.pid; do
    if [ -f "$SPARK_APP_DIR/$_pf" ]; then
      kill "$(cat "$SPARK_APP_DIR/$_pf")" 2>/dev/null || true
      rm -f "$SPARK_APP_DIR/$_pf"
    fi
  done
  ray stop --force >/dev/null 2>&1 || true
}
# ---- spark-cluster-ray end ----

case "${1:-}" in
  install)
    # 先校验集群信息与并行参数，免得下载完几百 GB 才发现配错
    cluster_load
    download
    ;;
  start)
    cluster_load
    if [ "$SPARK_CLUSTER_ROLE" = worker ]; then
      cluster_start_worker
      echo "$CLUSTER_SERVICE_PID" > "$SPARK_APP_DIR/service.pid"
      echo "ray worker started (head $SPARK_CLUSTER_HEAD_IP), pid $CLUSTER_SERVICE_PID"
      exit 0
    fi
    _dir=$(model_dir)
    # 只监听容器内回环：经平台 SSH 上游访问（端口只在 head 实例上登记）。
    # 接口密钥经环境变量 VLLM_API_KEY 传给 vLLM（未给 --api-key 时 vLLM 读它），不放进命令行；留空则不鉴权
    if [ -n "${API_KEY:-}" ]; then export VLLM_API_KEY="$API_KEY"; else unset VLLM_API_KEY; fi
    set -- vllm serve "$_dir" --host 127.0.0.1 --port "$SPARK_PORT" --served-model-name "${SERVED_MODEL_NAME:-model}" \
      --max-model-len "${MAX_MODEL_LEN:-32768}" --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.85}" \
      --max-num-seqs "${MAX_NUM_SEQS:-64}"
    if [ "${QUANTIZATION:-none}" != none ]; then set -- "$@" --quantization "$QUANTIZATION"; fi
    if [ "${REASONING_PARSER:-none}" != none ]; then set -- "$@" --reasoning-parser "$REASONING_PARSER"; fi
    # 工具调用：请求带 tool_choice=auto 时 vLLM 不开 auto tool choice 会回 400；hermes 适用于 Qwen 系列
    if [ "${TOOL_CALL_PARSER:-hermes}" != none ]; then set -- "$@" --enable-auto-tool-choice --tool-call-parser "${TOOL_CALL_PARSER:-hermes}"; fi
    # 每次启动换新的 vllm.log（上一次的留一份 vllm.log.1）：下面的 tail -n 200 只并入本次运行的输出
    if [ -f "$SPARK_APP_DIR/vllm.log" ]; then mv -f "$SPARK_APP_DIR/vllm.log" "$SPARK_APP_DIR/vllm.log.1"; fi
    cluster_start_head "$@"
    echo "$CLUSTER_SERVICE_PID" > "$SPARK_APP_DIR/service.pid"
    echo "ray head started, vllm serve (after $SPARK_CLUSTER_SIZE nodes join) pid $CLUSTER_SERVICE_PID; log: $SPARK_APP_DIR/vllm.log"
    # 把 vLLM 自己的日志尾部也并进部署日志（失败时能看到报错）
    ( tail -n 200 -f "$SPARK_APP_DIR/vllm.log" 2>/dev/null & echo $! > "$SPARK_APP_DIR/tail.pid" )
    ;;
  stop)
    cluster_stop
    ;;
esac
