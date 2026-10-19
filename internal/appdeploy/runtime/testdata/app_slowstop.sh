#!/bin/sh
# stop 分支用制表符缩进、写成 restart|stop)，并且卡住：run.sh stop 必须限时
case "$1" in
  install) ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
	restart|stop)
		echo stopping > "$SPARK_APP_DIR/stop.marker"
		exec sleep 100
		;;
esac
