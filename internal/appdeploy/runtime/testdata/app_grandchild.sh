#!/bin/sh
# 服务再起一个孙进程（写 grandchild.pid），用来验证失败路径会清掉整个进程组。
# GC_MODE: hang = 服务常驻（配合健康检查超时）；exit = 服务立刻退出、孙进程留着；fail = start 留下孙进程后返回 1
case "$1" in
  install) ;;
  start)
    case "${GC_MODE:-hang}" in
      fail)
        sleep 100000 </dev/null >/dev/null 2>&1 &
        echo $! > "$SPARK_APP_DIR/grandchild.pid"
        exit 1
        ;;
      exit)
        sh -c 'sleep 100000 </dev/null >/dev/null 2>&1 & echo $! > "$SPARK_APP_DIR/grandchild.pid"' </dev/null >/dev/null 2>&1 &
        echo $! > "$SPARK_APP_DIR/service.pid"
        ;;
      *)
        sh -c 'sleep 100000 </dev/null >/dev/null 2>&1 & echo $! > "$SPARK_APP_DIR/grandchild.pid"; wait' </dev/null >/dev/null 2>&1 &
        echo $! > "$SPARK_APP_DIR/service.pid"
        ;;
    esac
    ;;
esac
