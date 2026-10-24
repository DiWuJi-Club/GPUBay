#!/bin/sh
set -e
case "$1" in
  install)
    spark_status download 50 "downloading $MODEL_ID"
    sleep "${INSTALL_SLEEP:-0}"
    echo "installed $MODEL_ID" > "$SPARK_APP_DIR/installed.marker"
    ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "started pid $!"
    ;;
  stop) ;;
esac
