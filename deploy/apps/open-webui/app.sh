#!/bin/sh
# Open WebUI：install = 从 ModelScope 下载知识库用的向量模型；start = open-webui serve（独立 venv /opt/open-webui）放后台并写 service.pid。
# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用，env 里的变量已导出，spark_status 在 PATH 上。
set -eu

# 向量模型（RAG）：Open WebUI 默认启动时从 HuggingFace 拉 sentence-transformers/all-MiniLM-L6-v2，
# 经 hf-mirror 会被重定向到 HF 的 xet CDN（国内不可达），启动卡死在健康检查。改为 install 时从 ModelScope
# 下载到数据盘，start 时指向本地目录并开 OFFLINE_MODE（不再访问 HuggingFace，也不检查版本更新）。
EMBED_ID=sentence-transformers/all-MiniLM-L6-v2
EMBED_DIR=$SPARK_MODELS_DIR/modelscope/$EMBED_ID
OPEN_WEBUI_BIN=${OPEN_WEBUI_BIN:-/opt/open-webui/bin/open-webui} # 测试可替换

case "${1:-}" in
  install)
    spark_status download 20 "下载向量模型 ${EMBED_ID}（约 90MB）"
    mkdir -p "$EMBED_DIR"
    # 只要 safetensors 与配置；下载失败不阻塞部署：对话照常可用，只是知识库（文档检索）不可用
    if modelscope download "$EMBED_ID" --local-dir "$EMBED_DIR" \
        --exclude 'onnx/*' 'openvino/*' '*.bin' '*.ot' '*.h5' '*.msgpack' >/dev/null 2>&1; then
      spark_status install 60 "向量模型就绪"
    else
      echo "warning: 向量模型 ${EMBED_ID} 下载失败，知识库（文档检索）功能不可用；对话不受影响" >&2
      spark_status install 60 "向量模型下载失败，跳过"
    fi
    ;;
  start)
    # 平台没配置 HF 镜像时 HF_ENDPOINT 是空串，huggingface_hub 会拼出无效地址：空就去掉，用默认值
    [ -n "${HF_ENDPOINT:-}" ] || unset HF_ENDPOINT
    DATA_DIR=$SPARK_APP_DIR/data
    mkdir -p "$DATA_DIR"
    # 会话密钥：用户没填就用首次生成并保存在 .secret 里的（0600），重启后登录态不失效
    if [ -z "${WEBUI_SECRET_KEY:-}" ]; then
      if [ ! -s "$SPARK_APP_DIR/.secret" ]; then
        (umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$SPARK_APP_DIR/.secret")
      fi
      WEBUI_SECRET_KEY=$(cat "$SPARK_APP_DIR/.secret")
    fi
    WEBUI_AUTH=True
    OFFLINE_MODE=True
    export DATA_DIR WEBUI_SECRET_KEY WEBUI_AUTH OPENAI_API_BASE_URL OPENAI_API_KEY OFFLINE_MODE
    if [ -f "$EMBED_DIR/model.safetensors" ]; then
      RAG_EMBEDDING_MODEL=$EMBED_DIR
      export RAG_EMBEDDING_MODEL
    fi
    "$OPEN_WEBUI_BIN" serve --host 127.0.0.1 --port "$SPARK_PORT" </dev/null >>"$SPARK_APP_DIR/webui.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "open-webui started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/webui.log"
    ;;
esac
