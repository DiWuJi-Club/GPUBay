package appdeploy

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
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
	// Extra 调用方追加的文件（计划 11：comfywf 生成的 workflows/…），按给定顺序写在固定文件之后；名字规则见 extraNames.check。
	Extra []BundleFile
	// 计划 13：集群部署的成员（head / worker 与 acd-…）；非集群部署为空，env 与改动前逐字节相同。
	ClusterRole, ClusterDeploymentID string
}

// BundleFile 是部署包里由调用方追加的额外文件（如 comfywf 生成的 workflows/manifest.json 与 workflows/<slug>.json）。
// Name 为包内相对路径，Mode 为 tar 里的权限位（如 0o644），Body 为文件内容。
type BundleFile struct {
	Name string
	Mode int64
	Body []byte
}

func AppDir(appRoot, slug string) string { return path.Join(appRoot, AppDirName, slug) }

// ReservedEnv 平台注入的保留变量（顺序固定，写在用户变量之前；SPARK_GIT_MIRROR 只对 Spec.SupportsWorkflows 的版本；
// SPARK_CLUSTER_ROLE / SPARK_CLUSTER_DEPLOYMENT 只对集群部署（ClusterDeploymentID 非空），在最后）。
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
	kvs := []EnvKV{
		{"SPARK_APP", in.Slug}, {"SPARK_APP_DIR", AppDir(in.AppRoot, in.Slug)}, {"SPARK_DATA_DIR", in.AppRoot},
		{"SPARK_PORT", strconv.Itoa(in.Spec.Ports[0].Port)}, {"SPARK_PORT_2", port2}, {"SPARK_INSTANCE_ID", in.InstanceID},
		{"SPARK_MODELS_DIR", models}, {"SPARK_SPEC_HASH", in.SpecHash}, {"SPARK_TIMEOUT_S", strconv.Itoa(in.Spec.TimeoutMinutes * 60)},
		{"SPARK_HEALTH_PATH", in.Spec.Ports[0].HealthPath}, {"SPARK_MIN_FREE_GIB", strconv.Itoa(in.Spec.MinDataFreeGiB)}, {"SPARK_HEALTH_ADDR", health},
		{"HF_ENDPOINT", in.HFEndpoint}, {"MODELSCOPE_CACHE", path.Join(models, "modelscope")}, {"HF_HOME", path.Join(models, "hf")}, {"PIP_INDEX_URL", in.PipIndexURL},
	}
	// 计划 11：只给支持工作流的版本。其它部署的 env 必须与计划 7 逐字节相同：env 的 cksum 是 run.sh install_key 的一部分，
	// 多一行会让所有存量部署在升级 platform 后的第一次恢复里重跑 install。
	if in.Spec.SupportsWorkflows {
		kvs = append(kvs, EnvKV{"SPARK_GIT_MIRROR", in.GitMirror})
	}
	// 计划 13 §6.3：只对集群部署（同样的理由：非集群部署多一行就会触发存量重装）。集群拓扑由 app.sh 读 /.spark/cluster.env。
	if in.ClusterDeploymentID != "" {
		kvs = append(kvs, EnvKV{"SPARK_CLUSTER_ROLE", in.ClusterRole}, EnvKV{"SPARK_CLUSTER_DEPLOYMENT", in.ClusterDeploymentID})
	}
	return kvs
}

// ExtraDir 是 Extra 文件唯一允许的包内目录（UploadCommand 每次解包前清空它，app.sh 只从这里读）。
const ExtraDir = "workflows"

var extraNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9/._-]{0,127}$`)

// extraNames 记录已加入的 Extra 文件与它们的上级目录，用于拒绝重名与「同一路径既是文件又是目录」（x 与 x/y）。
type extraNames struct{ files, dirs map[string]bool }

// check：包内相对路径，字符集 [a-z0-9/._-]、首字符字母或数字、≤ 128，必须在 workflows/ 下（因此不会与固定文件冲突）；
// 每段非空且不是 . / ..（不能逃出应用目录）；不与已有 Extra 重名，也不与已有 Extra 的文件 / 目录互为前缀。
func (n *extraNames) check(name string) error {
	if !extraNameRe.MatchString(name) || !strings.HasPrefix(name, ExtraDir+"/") {
		return fmt.Errorf("appdeploy: bad extra file name %q", name)
	}
	segs := strings.Split(name, "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("appdeploy: bad extra file name %q", name)
		}
	}
	if n.files[name] || n.dirs[name] {
		return fmt.Errorf("appdeploy: duplicate bundle file %q", name)
	}
	for k := 1; k < len(segs); k++ {
		if dir := strings.Join(segs[:k], "/"); n.files[dir] {
			return fmt.Errorf("appdeploy: bundle file %q is under file %q", name, dir)
		}
	}
	n.files[name] = true
	for k := 1; k < len(segs); k++ {
		n.dirs[strings.Join(segs[:k], "/")] = true
	}
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
	names := extraNames{files: map[string]bool{}, dirs: map[string]bool{}}
	for _, f := range in.Extra {
		if err := names.check(f.Name); err != nil {
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
