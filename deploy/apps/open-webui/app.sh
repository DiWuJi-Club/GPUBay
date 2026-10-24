#!/bin/sh
# Open WebUI：install 无需下载；start = open-webui serve（独立 venv /opt/open-webui）放后台并写 service.pid。
# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用，env 里的变量已导出，spark_status 在 PATH 上。
set -eu

case "${1:-}" in
  install)
    spark_status install 50 "无需下载"
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
    export DATA_DIR WEBUI_SECRET_KEY WEBUI_AUTH OPENAI_API_BASE_URL OPENAI_API_KEY
    /opt/open-webui/bin/open-webui serve --host 127.0.0.1 --port "$SPARK_PORT" </dev/null >>"$SPARK_APP_DIR/webui.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "open-webui started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/webui.log"
    ;;
esac
