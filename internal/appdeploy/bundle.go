package appdeploy

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type BundleInput struct {
	Slug, InstanceID, AppRoot, SpecHash string
	Spec                                Spec
	Script                              string
	Env, Secret                         map[string]string
	HealthAddr                          string // 空 = 127.0.0.1:<主端口>
	HFEndpoint, PipIndexURL             string
	GitMirror                           string // 注入 SPARK_GIT_MIRROR（计划 11；空 = 直连）
	// ImageEnv 镜像环境（imageenv.go）：非 nil（含空切片）时包里带 image.env（0600）；nil = 没读到，不带 image.env
	// （目录里上一次部署留下的 image.env 保留：同一实例的镜像不会变）。
	ImageEnv []EnvKV
	// Extra 调用方追加的文件（计划 11：comfywf 生成的 workflows/…），按给定顺序写在固定文件之后；名字规则见 checkExtraName。
	Extra []BundleFile
}

// BundleFile 是部署包里由调用方追加的额外文件（如 comfywf 生成的 workflows/manifest.json 与 workflows/<slug>.json）。
// Name 为包内相对路径，Mode 为 tar 里的权限位（如 0o644），Body 为文件内容。
type BundleFile struct {
	Name string
	Mode int64
	Body []byte
}

func AppDir(appRoot, slug string) string { return path.Join(appRoot, AppDirName, slug) }

// ReservedEnv 平台注入的保留变量（顺序固定，写在用户变量之前）。
func ReservedEnv(in BundleInput) []EnvKV {
	models := path.Join(in.AppRoot, ModelsDirName)
	port2 := ""
	if len(in.Spec.Ports) > 1 {
		port2 = strconv.Itoa(in.Spec.Ports[1].Port)
	}
	health := in.HealthAddr
	if health == "" {
		health = "127.0.0.1:" + strconv.Itoa(in.Spec.Ports[0].Port)
	}
	return []EnvKV{
		{"SPARK_APP", in.Slug}, {"SPARK_APP_DIR", AppDir(in.AppRoot, in.Slug)}, {"SPARK_DATA_DIR", in.AppRoot},
		{"SPARK_PORT", strconv.Itoa(in.Spec.Ports[0].Port)}, {"SPARK_PORT_2", port2}, {"SPARK_INSTANCE_ID", in.InstanceID},
		{"SPARK_MODELS_DIR", models}, {"SPARK_SPEC_HASH", in.SpecHash}, {"SPARK_TIMEOUT_S", strconv.Itoa(in.Spec.TimeoutMinutes * 60)},
		{"SPARK_HEALTH_PATH", in.Spec.Ports[0].HealthPath}, {"SPARK_MIN_FREE_GIB", strconv.Itoa(in.Spec.MinDataFreeGiB)}, {"SPARK_HEALTH_ADDR", health},
		{"HF_ENDPOINT", in.HFEndpoint}, {"MODELSCOPE_CACHE", path.Join(models, "modelscope")}, {"HF_HOME", path.Join(models, "hf")}, {"PIP_INDEX_URL", in.PipIndexURL},
		{"SPARK_GIT_MIRROR", in.GitMirror},
	}
}

// fixedBundleFiles 是 Bundle 自己写的文件名；Extra 不得与之重名。
var fixedBundleFiles = []string{"run.sh", "app.sh", "spec.json", "env", "image.env"}

var extraNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9/._-]{0,127}$`)

// checkExtraName：包内相对路径，字符集 [a-z0-9/._-]、首字符字母或数字、≤ 128；每段非空且不是 . / ..（不能逃出应用目录）；
// 不与固定文件、其它 Extra 重名。
func checkExtraName(name string, seen map[string]bool) error {
	if !extraNameRe.MatchString(name) {
		return fmt.Errorf("appdeploy: bad extra file name %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("appdeploy: bad extra file name %q", name)
		}
	}
	top, _, nested := strings.Cut(name, "/")
	if seen[name] || (nested && slices.Contains(fixedBundleFiles, top)) {
		return fmt.Errorf("appdeploy: duplicate bundle file %q", name)
	}
	seen[name] = true
	return nil
}

// Bundle 生成部署包 tar（run.sh 0755、app.sh 0644、spec.json 0644、env 0600、读到镜像环境时 image.env 0600，之后是 Extra；
// mtime 固定为 0，输出可重复）。
func Bundle(in BundleInput, runSH []byte) ([]byte, error) {
	if !ValidSlug(in.Slug) {
		return nil, errors.New("appdeploy: bad slug")
	}
	if len(in.Spec.Ports) == 0 {
		return nil, errors.New("appdeploy: spec has no ports")
	}
	specJSON, err := json.Marshal(in.Spec)
	if err != nil {
		return nil, err
	}
	user := append(sortedKV(in.Env), sortedKV(in.Secret)...)
	if err := checkUserEnvNames(user); err != nil {
		return nil, err
	}
	env := RenderEnv(ReservedEnv(in), user)
	type entry struct {
		name string
		mode int64
		body []byte
	}
	entries := []entry{{"run.sh", 0o755, runSH}, {"app.sh", 0o644, []byte(in.Script)}, {"spec.json", 0o644, specJSON}, {"env", 0o600, env}}
	if in.ImageEnv != nil {
		entries = append(entries, entry{"image.env", 0o600, RenderImageEnv(in.ImageEnv)})
	}
	seen := map[string]bool{}
	for _, n := range fixedBundleFiles {
		seen[n] = true
	}
	for _, f := range in.Extra {
		if err := checkExtraName(f.Name, seen); err != nil {
			return nil, err
		}
		mode := f.Mode & 0o777
		if mode == 0 {
			mode = 0o644
		}
		entries = append(entries, entry{f.Name, mode, f.Body})
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range entries {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Uid: 0, Gid: 0, Format: tar.FormatPAX}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(f.body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() > MaxBundleBytes {
		return nil, fmt.Errorf("appdeploy: 部署包 %d 字节超过上限 %d", buf.Len(), MaxBundleBytes)
	}
	return buf.Bytes(), nil
}

// UploadCommand 上传部署包的固定命令；dir 只由 AppRoot 与 slug 拼出，命令行里没有任何用户输入；仍按单引号转义，AppRoot 配置异常也不会被 sh 解释。
// image.env 不一定在（本次没读到镜像环境、之前也没有）：在才 chmod。
// 解包前删掉 workflows/（计划 11）：集合里移除的工作流不会残留在包目录；本次包里有的随 tar 重新写出。
func UploadCommand(dir string) string {
	q, ie := ShellQuote(dir), ShellQuote(dir+"/image.env")
	return "mkdir -p " + q + " && chmod 700 " + q + " && rm -rf " + ShellQuote(dir+"/workflows") + " && tar -x -C " + q + " -f - && chmod 600 " + ShellQuote(dir+"/env") +
		" && { [ ! -e " + ie + " ] || chmod 600 " + ie + "; }"
}
