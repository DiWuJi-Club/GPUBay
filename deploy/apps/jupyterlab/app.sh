#!/bin/sh
# JupyterLab：install 无需下载；start = jupyter lab 放后台并写 service.pid。
# 由平台 run.sh 以 `sh app.sh install|start|stop` 调用，env 里的变量已导出，spark_status 在 PATH 上。
set -eu

case "${1:-}" in
  install)
    spark_status install 50 "无需下载"
    ;;
  start)
    jupyter lab --ip 127.0.0.1 --port "$SPARK_PORT" --no-browser --allow-root --notebook-dir "$SPARK_DATA_DIR" \
      --IdentityProvider.token="$JUPYTER_TOKEN" --ServerApp.allow_remote_access=True --ServerApp.base_url=/ \
      </dev/null >>"$SPARK_APP_DIR/jupyter.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "jupyter lab started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/jupyter.log"
    ;;
esac
