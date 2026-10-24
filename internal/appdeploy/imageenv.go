package appdeploy

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 镜像环境（验收缺陷 A-1）：平台经实例 sshd 执行 run.sh，SSH 会话不继承镜像的 Docker ENV（CUDA_HOME、LD_LIBRARY_PATH、
// NVIDIA_*、TRITON_* …）。Agent 的容器入口 /.spark/init.sh 在 exec sshd 之前把自己的环境（= 镜像 ENV + docker run 的 -e）
// 原样拷到 DefaultImageEnviron（NAME=value\0…）。不读 /proc/1/environ：PID 1 是 sshd，OpenSSH 在 Linux 上的
// setproctitle 复用 argv + environ 的内存写进程标题（「sshd: … [listener] …」）并用 NUL 填满，/proc/1/environ 只剩标题残片。
// Runner 部署前经 Pool.Exec 读该文件（base64 传输，NUL 不丢），在这里过滤后写成部署包里的 image.env，
// run.sh 先加载 image.env 再加载 env（平台保留变量与用户变量覆盖镜像的同名变量）。
const (
	DefaultImageEnviron  = "/run/spark-image.environ"
	MaxImageEnvironBytes = 49152 // 读取上限；base64 后约 65KB，在 exec 输出上限之内
	MaxImageEnvVars      = 256
	MaxImageEnvBytes     = 65536 // image.env 渲染后的总量上限
)

var imageEnvNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// imageEnvDeny：会话 / 容器自身的变量（不是镜像配置，或会改变 sh 的行为），bash 的只读变量（sh 是 bash 时 export 只读变量
// 会让非交互 shell 直接退出），以及平台保留的 SPARK_*（另按前缀过滤）。
var imageEnvDeny = map[string]bool{"HOSTNAME": true, "HOME": true, "PWD": true, "OLDPWD": true, "SHLVL": true, "TERM": true, "_": true,
	"IFS": true, "ENV": true, "BASH_ENV": true, "CDPATH": true, "POSIXLY_CORRECT": true, "PS4": true,
	"PPID": true, "UID": true, "EUID": true, "SHELLOPTS": true, "BASHOPTS": true, "BASH_VERSINFO": true, "BASHPID": true, "GROUPS": true}

// imageEnvironCmd 读容器里 init.sh 落下的镜像环境（截到 MaxImageEnvironBytes）并 base64 输出；文件不可读时非零退出。
func imageEnvironCmd(path string) string {
	q := ShellQuote(path)
	return "[ -r " + q + " ] && head -c " + strconv.Itoa(MaxImageEnvironBytes) + " " + q + " | base64"
}

// DecodeImageEnviron 解码 imageEnvironCmd 的输出（base64，GNU base64 每 76 列换行，解码时忽略换行）并解析。
func DecodeImageEnviron(out []byte) ([]EnvKV, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(out)))
	if err != nil {
		return nil, fmt.Errorf("image environ: %w", err)
	}
	return ParseImageEnviron(raw), nil
}

// ParseImageEnviron 解析 environ 格式的原文（NAME=value\0…）：最后一段没有 NUL 结尾（被截断）丢弃；名字须匹配
// ^[A-Za-z_][A-Za-z0-9_]*$ 且不在 imageEnvDeny、不以 SPARK_ 开头；同名取第一个；最多 MaxImageEnvVars 项，渲染后总量超过
// MaxImageEnvBytes 的项跳过。返回值非 nil（读到了但为空时也写出空的 image.env）。
func ParseImageEnviron(raw []byte) []EnvKV {
	out := []EnvKV{}
	parts := bytes.Split(raw, []byte{0})
	parts = parts[:len(parts)-1] // 正常以 NUL 结尾时最后一段是空串；截断时是半个条目
	seen := map[string]bool{}
	size := 0
	for _, p := range parts {
		name, value, ok := strings.Cut(string(p), "=")
		if !ok || !imageEnvNameRe.MatchString(name) || imageEnvDeny[name] || strings.HasPrefix(name, "SPARK_") || seen[name] {
			continue
		}
		seen[name] = true
		n := len(renderLine(name, value))
		if size+n > MaxImageEnvBytes {
			continue
		}
		size += n
		out = append(out, EnvKV{name, value})
		if len(out) == MaxImageEnvVars {
			break
		}
	}
	return out
}

func renderLine(name, value string) string { return "export " + name + "=" + ShellQuote(value) + "\n" }

// RenderImageEnv 生成 image.env：每行 export NAME='value'；名字不合规的不写出（纵深防御，值里不会有 NUL）。
func RenderImageEnv(kvs []EnvKV) []byte {
	var b strings.Builder
	for _, kv := range kvs {
		if imageEnvNameRe.MatchString(kv.Name) && !strings.ContainsRune(kv.Value, 0) {
			b.WriteString(renderLine(kv.Name, kv.Value))
		}
	}
	return []byte(b.String())
}
