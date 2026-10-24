#!/bin/sh
# 计划 11 D7：模型下载前磁盘预检失败 → 先汇报 disk: 消息再非零退出
case "$1" in
  install) spark_status download 10 "disk: 剩余 1GiB，需要 12GiB"; exit 2 ;;
  start) exit 1 ;;
esac
