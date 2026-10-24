#!/bin/sh
# ComfyUI：install = 下载所选模型到 $SPARK_MODELS_DIR 并软链进 /opt/ComfyUI/models；start = main.py 放后台并写 service.pid。
# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用，env 里的变量已导出，spark_status 在 PATH 上。
set -eu

# 平台没配置 HF 镜像时 HF_ENDPOINT 是空串，huggingface_hub 会拼出无效地址：空就去掉，用默认值
[ -n "${HF_ENDPOINT:-}" ] || unset HF_ENDPOINT
# 下载工具的进度条（\r 刷屏）不进日志，进度由 spark_status 汇报
export TQDM_DISABLE=1 HF_HUB_DISABLE_PROGRESS_BARS=1

COMFY=/opt/ComfyUI

source_dir() {
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) echo "$SPARK_MODELS_DIR/hf" ;;
    *) echo "$SPARK_MODELS_DIR/modelscope" ;;
  esac
}

# 所选模型要下载的文件，每行「仓库 仓库内路径 ComfyUI 模型子目录」。
# 只下 ComfyUI 能直接加载的单文件权重：仓库里的 diffusers 分片格式（transformer/ text_encoder/ 下的多分片）ComfyUI 读不了，体积也翻倍。
model_files() {
  case "${MODEL_SOURCE:-modelscope}:$MODEL_ID" in
    modelscope:AI-ModelScope/stable-diffusion-xl-base-1.0)
      echo "AI-ModelScope/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors checkpoints" ;;
    hf-mirror:AI-ModelScope/stable-diffusion-xl-base-1.0)
      echo "stabilityai/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors checkpoints" ;;
    modelscope:AI-ModelScope/FLUX.1-schnell)
      echo "AI-ModelScope/FLUX.1-schnell flux1-schnell.safetensors unet"
      echo "AI-ModelScope/FLUX.1-schnell ae.safetensors vae"
      echo "AI-ModelScope/flux_text_encoders clip_l.safetensors clip"
      echo "AI-ModelScope/flux_text_encoders t5xxl_fp16.safetensors clip" ;;
    hf-mirror:AI-ModelScope/FLUX.1-schnell)
      # HF 上的 black-forest-labs/FLUX.1-schnell 要登录同意协议（hf-mirror 返回 403），用 Comfy-Org 打包的 fp8 单文件 checkpoint
      echo "Comfy-Org/flux1-schnell flux1-schnell-fp8.safetensors checkpoints" ;;
    *:Qwen/Qwen-Image)
      # Comfy-Org 重新打包的 ComfyUI 版本（ModelScope 与 HF 同名）；fp8 是 ComfyUI 官方示例的默认组合
      echo "Comfy-Org/Qwen-Image_ComfyUI split_files/diffusion_models/qwen_image_fp8_e4m3fn.safetensors unet"
      echo "Comfy-Org/Qwen-Image_ComfyUI split_files/text_encoders/qwen_2.5_vl_7b_fp8_scaled.safetensors clip"
      echo "Comfy-Org/Qwen-Image_ComfyUI split_files/vae/qwen_image_vae.safetensors vae" ;;
    *)
      echo "不支持的模型 $MODEL_ID（下载源 ${MODEL_SOURCE:-modelscope}）" >&2
      return 1 ;;
  esac
}

# 模型大小（GB）供进度估算
model_size_gb() {
  case "${MODEL_SOURCE:-modelscope}:$MODEL_ID" in
    *:AI-ModelScope/stable-diffusion-xl-base-1.0) echo 7 ;;
    hf-mirror:AI-ModelScope/FLUX.1-schnell) echo 17 ;;
    *:AI-ModelScope/FLUX.1-schnell) echo 34 ;;
    *:Qwen/Qwen-Image) echo 30 ;;
    *) echo 10 ;;
  esac
}

# 后台每 10 秒按 du 汇报下载百分比（5～75）；参数是本模型涉及的仓库目录（下载中的 .incomplete 文件也在里面）
progress_loop() {
  _total_kb=$(( $(model_size_gb) * 1024 * 1024 ))
  while :; do
    sleep 10
    _kb=$(du -skc "$@" 2>/dev/null | tail -n 1 | awk '{print $1}')
    _pct=$(( 5 + ${_kb:-0} * 70 / _total_kb ))
    [ "$_pct" -gt 75 ] && _pct=75
    spark_status download "$_pct" "下载模型 $MODEL_ID：$(( ${_kb:-0} / 1024 / 1024 ))GB / $(model_size_gb)GB"
  done
}

fetch() { # fetch <仓库> <仓库内路径> <本地目录>：已下载完整的文件会跳过
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) hf download "$1" "$2" --local-dir "$3" </dev/null ;;
    *) modelscope download "$1" "$2" --local-dir "$3" </dev/null ;;
  esac
}

# 把已下载的文件软链进 ComfyUI 的模型目录。/opt/ComfyUI 在镜像层里，容器重建后软链会丢，所以 start 时也重做一遍
link_models() {
  _base=$(source_dir)
  model_files | while read -r _repo _file _sub; do
    if [ -f "$_base/$_repo/$_file" ]; then
      mkdir -p "$COMFY/models/$_sub"
      ln -sfn "$_base/$_repo/$_file" "$COMFY/models/$_sub/"
    fi
  done
}

download() {
  _base=$(source_dir)
  _files=$(model_files)
  # 涉及的仓库目录（去重）作为进度循环的参数
  set -- $(printf '%s\n' "$_files" | awk -v b="$_base" '!seen[$1]++ {print b "/" $1}')
  spark_status download 5 "开始下载 $MODEL_ID（约 $(model_size_gb)GB）"
  progress_loop "$@" &
  _pl=$!
  # 下载失败（set -e 退出）时也要停掉进度循环，否则它会在 run.sh 写 failed 之后继续写 installing
  trap 'kill "$_pl" 2>/dev/null || true' EXIT
  printf '%s\n' "$_files" | while read -r _repo _file _sub; do
    fetch "$_repo" "$_file" "$_base/$_repo"
  done
  kill "$_pl" 2>/dev/null || true
  trap - EXIT
  link_models
  spark_status install 78 "模型就绪"
}

case "${1:-}" in
  install)
    download
    ;;
  start)
    link_models
    mkdir -p "$SPARK_DATA_DIR/comfyui-output"
    cd "$COMFY"
    python3 main.py --listen 127.0.0.1 --port "$SPARK_PORT" --output-directory "$SPARK_DATA_DIR/comfyui-output" </dev/null >>"$SPARK_APP_DIR/comfyui.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "comfyui started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/comfyui.log"
    ;;
esac
