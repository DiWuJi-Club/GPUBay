#!/bin/sh
case "$1" in
  install) echo "boom: cannot download" >&2; exit 3 ;;
  start) echo "should not start"; exit 1 ;;
esac
