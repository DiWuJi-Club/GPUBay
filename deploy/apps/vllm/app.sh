#!/bin/sh
# vLLM 推理服务：install = 下载模型到 $SPARK_MODELS_DIR；start = vllm serve 放后台并写 service.pid。
# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用，env 里的变量已导出，spark_status 在 PATH 上。
set -eu

# 平台没配置 HF 镜像时 HF_ENDPOINT 是空串，huggingface_hub 会拼出无效地址：空就去掉，用默认值
[ -n "${HF_ENDPOINT:-}" ] || unset HF_ENDPOINT
# 下载工具的进度条（\r 刷屏）不进日志，进度由 spark_status 汇报
export TQDM_DISABLE=1 HF_HUB_DISABLE_PROGRESS_BARS=1

# 模型大小（GB）供进度估算；spec 的 options 不带大小
model_size_gb() {
  case "$1" in
    Qwen/Qwen3-4B) echo 8 ;;
    Qwen/Qwen3-8B) echo 16 ;;
    Qwen/Qwen2.5-7B-Instruct) echo 15 ;;
    Qwen/Qwen3-14B) echo 29 ;;
    *) echo 16 ;;
  esac
}

model_dir() {
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) echo "$SPARK_MODELS_DIR/hf/$MODEL_ID" ;;
    *) echo "$SPARK_MODELS_DIR/modelscope/$MODEL_ID" ;;
  esac
}

# 后台每 10 秒按 du 汇报下载百分比（5～75）
progress_loop() {
  _dir=$1; _total_kb=$(( $(model_size_gb "$MODEL_ID") * 1024 * 1024 ))
  while :; do
    sleep 10
    _kb=$(du -sk "$_dir" 2>/dev/null | awk '{print $1}')
    _pct=$(( 5 + ${_kb:-0} * 70 / _total_kb ))
    [ "$_pct" -gt 75 ] && _pct=75
    spark_status download "$_pct" "下载模型 $MODEL_ID：$(( ${_kb:-0} / 1024 / 1024 ))GB / $(model_size_gb "$MODEL_ID")GB"
  done
}

download() {
  _dir=$(model_dir)
  mkdir -p "$_dir"
  spark_status download 5 "开始下载 $MODEL_ID（约 $(model_size_gb "$MODEL_ID")GB）"
  progress_loop "$_dir" &
  _pl=$!
  # 下载失败（set -e 退出）时也要停掉进度循环，否则它会在 run.sh 写 failed 之后继续写 installing
  trap 'kill "$_pl" 2>/dev/null || true' EXIT
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror)
      # HF_ENDPOINT 由平台注入（hf-mirror）；hf 命令随 huggingface_hub 安装
      hf download "$MODEL_ID" --local-dir "$_dir" ;;
    *)
      # 已下载完整的文件会跳过（重新部署 / 换回旧模型秒过）
      modelscope download "$MODEL_ID" --local-dir "$_dir" ;;
  esac
  kill "$_pl" 2>/dev/null || true
  trap - EXIT
  spark_status install 78 "模型就绪"
}

case "${1:-}" in
  install)
    download
    ;;
  start)
    _dir=$(model_dir)
    # 工具调用：Open WebUI 默认 function_calling = native，请求带 tool_choice=auto，vLLM 不开 auto tool choice 会回 400；
    # hermes 解析器适用于目录里的 Qwen3 / Qwen2.5
    set -- vllm serve "$_dir" --host 127.0.0.1 --port "$SPARK_PORT" --served-model-name "$MODEL_ID" \
      --max-model-len "${MAX_MODEL_LEN:-32768}" --gpu-memory-utilization "${GPU_MEMORY_UTILIZATION:-0.7}" \
      --enable-auto-tool-choice --tool-call-parser hermes
    if [ -n "${API_KEY:-}" ]; then set -- "$@" --api-key "$API_KEY"; fi
    # 每次启动换新的 vllm.log（上一次的留一份 vllm.log.1）：下面的 tail -n 200 只并入本次运行的输出
    if [ -f "$SPARK_APP_DIR/vllm.log" ]; then mv -f "$SPARK_APP_DIR/vllm.log" "$SPARK_APP_DIR/vllm.log.1"; fi
    "$@" </dev/null >>"$SPARK_APP_DIR/vllm.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "vllm serve started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/vllm.log"
    # 把 vLLM 自己的日志尾部也并进部署日志，方便控制台排障（失败时能看到报错）
    ( tail -n 200 -f "$SPARK_APP_DIR/vllm.log" 2>/dev/null & echo $! > "$SPARK_APP_DIR/tail.pid" )
    ;;
  stop)
    [ -f "$SPARK_APP_DIR/tail.pid" ] && kill "$(cat "$SPARK_APP_DIR/tail.pid")" 2>/dev/null || true
    ;;
esac
