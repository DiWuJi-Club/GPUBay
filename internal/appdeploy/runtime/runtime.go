// Package runtime 内嵌容器内的固定包装脚本 run.sh（随 platform 二进制发布，版本脚本 app.sh 只做 install / start / stop）。
package runtime

import _ "embed"

//go:embed run.sh
var RunSH []byte
