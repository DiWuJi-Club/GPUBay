#!/bin/sh
# 记录 install / start 时看到的环境（镜像环境 image.env 与 env 的加载顺序测试用）
set -e
case "$1" in
  install) env > "$SPARK_APP_DIR/install.env" ;;
  start)
    env > "$SPARK_APP_DIR/start.env"
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
esac
