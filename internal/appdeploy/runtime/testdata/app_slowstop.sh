#!/bin/sh
# stop 分支用制表符缩进、写成 restart|stop)，并且卡住：run.sh stop 必须限时
case "$1" in
  install) ;;
  start)
    if [ "${IGNORE_TERM:-}" = 1 ]; then # 服务不理 TERM：kill_group 要等满再 KILL
      sh -c 'trap "" TERM; while :; do sleep 1; done' </dev/null >/dev/null 2>&1 &
    else
      sleep 100000 </dev/null >/dev/null 2>&1 &
    fi
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
	restart|stop)
		echo stopping > "$SPARK_APP_DIR/stop.marker"
		exec sleep 100
		;;
esac
