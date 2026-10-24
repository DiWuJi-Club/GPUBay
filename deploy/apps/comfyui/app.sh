#!/bin/sh
# ComfyUI 1.1.0：
#   install = ①MODEL_ID 不是 none 时下载所选预置模型（1.0.0 的逻辑）；②按部署包里的 workflows/manifest.json（平台生成，工作流市场）
#             补齐工作流需要的模型、自定义节点，把工作流文件放进用户目录；全部幂等，已有的秒过
#   start   = 生成 extra_model_paths.yaml，main.py 放后台并写 service.pid
# 模型软链、自定义节点、用户目录（含保存的工作流）、输入目录都在数据盘 $SPARK_DATA_DIR/comfyui 下，容器重建、卸载应用都不丢；
# 模型文件本身缓存在 ${SPARK_MODELS_DIR}（modelscope/<仓库>、hf/<仓库>）。
# 由平台 run.sh 以 `sh app.sh install|start` 调用，env 里的变量已导出，spark_status 在 PATH 上。
# 注意：变量后面紧跟中文时加花括号写成 ${_x} 形式（有的 sh 会把多字节字符当成变量名的一部分）。
set -eu

# 平台没配置 HF 镜像时 HF_ENDPOINT 是空串，huggingface_hub 会拼出无效地址：空就去掉，用默认值
[ -n "${HF_ENDPOINT:-}" ] || unset HF_ENDPOINT
# 下载工具的进度条（\r 刷屏）不进日志，进度由 spark_status 汇报
export TQDM_DISABLE=1 HF_HUB_DISABLE_PROGRESS_BARS=1
# 不走 Xet 存储协议（hf-mirror 是否支持 Xet 不确定），统一用普通 HTTP 下载
export HF_HUB_DISABLE_XET=1

# 镜像里的 ComfyUI 程序目录（SPARK_TEST_COMFY_DIR 只给平台单测用）
COMFY=${SPARK_TEST_COMFY_DIR:-/opt/ComfyUI}
CDATA=$SPARK_DATA_DIR/comfyui
OUTDIR=$SPARK_DATA_DIR/comfyui-output
WFDIR=$CDATA/user/default/workflows
MANIFEST=$SPARK_APP_DIR/workflows/manifest.json
# 模型子目录（与平台 comfywf.ModelDirs 一致）
MODEL_DIRS="checkpoints diffusion_models unet text_encoders clip vae loras upscale_models controlnet clip_vision embeddings style_models vae_approx"
MODEL=${MODEL_ID:-none}
# 进度分段：预置模型 5～PRESET_HI，工作流模型 WF_LO～70，节点 70～78，工作流文件 79（两者都有时预置模型只占 5～35，进度不回退）
PRESET_HI=75
WF_LO=5

log() { printf '%s\n' "$*"; }
die() { printf '%s\n' "$*" >&2; exit 1; }

ensure_dirs() {
  for _d in $MODEL_DIRS; do mkdir -p "$CDATA/models/$_d"; done
  mkdir -p "$CDATA/custom_nodes" "$CDATA/user" "$CDATA/input" "$OUTDIR"
}

# ---------- 预置模型（MODEL_ID，1.0.0 的逻辑；软链目标改为数据盘 comfyui/models/<子目录>） ----------

source_dir() {
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) echo "$SPARK_MODELS_DIR/hf" ;;
    *) echo "$SPARK_MODELS_DIR/modelscope" ;;
  esac
}

# 所选模型要下载的文件，每行「仓库 仓库内路径 ComfyUI 模型子目录」。
# 只下 ComfyUI 能直接加载的单文件权重：仓库里的 diffusers 分片格式（transformer/ text_encoder/ 下的多分片）ComfyUI 读不了，体积也翻倍。
model_files() {
  case "${MODEL_SOURCE:-modelscope}:$MODEL" in
    modelscope:AI-ModelScope/stable-diffusion-xl-base-1.0)
      echo "AI-ModelScope/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors checkpoints" ;;
    hf-mirror:AI-ModelScope/stable-diffusion-xl-base-1.0)
      echo "stabilityai/stable-diffusion-xl-base-1.0 sd_xl_base_1.0.safetensors checkpoints" ;;
    modelscope:AI-ModelScope/FLUX.1-schnell)
      echo "AI-ModelScope/FLUX.1-schnell flux1-schnell.safetensors unet"
      echo "AI-ModelScope/FLUX.1-schnell ae.safetensors vae"
      echo "AI-ModelScope/flux_text_encoders clip_l.safetensors clip"
      echo "AI-ModelScope/flux_text_encoders t5xxl_fp16.safetensors clip" ;;
    hf-mirror:AI-ModelScope/FLUX.1-schnell)
      # HF 上的 black-forest-labs/FLUX.1-schnell 要登录同意协议（hf-mirror 返回 403），用 Comfy-Org 打包的 fp8 单文件 checkpoint
      echo "Comfy-Org/flux1-schnell flux1-schnell-fp8.safetensors checkpoints" ;;
    *:Qwen/Qwen-Image)
      # Comfy-Org 重新打包的 ComfyUI 版本（ModelScope 与 HF 同名）；fp8 是 ComfyUI 官方示例的默认组合
      echo "Comfy-Org/Qwen-Image_ComfyUI split_files/diffusion_models/qwen_image_fp8_e4m3fn.safetensors unet"
      echo "Comfy-Org/Qwen-Image_ComfyUI split_files/text_encoders/qwen_2.5_vl_7b_fp8_scaled.safetensors clip"
      echo "Comfy-Org/Qwen-Image_ComfyUI split_files/vae/qwen_image_vae.safetensors vae" ;;
    *)
      echo "不支持的模型 ${MODEL}（下载源 ${MODEL_SOURCE:-modelscope}）" >&2
      return 1 ;;
  esac
}

# 模型大小（GB）供进度估算
model_size_gb() {
  case "${MODEL_SOURCE:-modelscope}:$MODEL" in
    *:AI-ModelScope/stable-diffusion-xl-base-1.0) echo 7 ;;
    hf-mirror:AI-ModelScope/FLUX.1-schnell) echo 17 ;;
    *:AI-ModelScope/FLUX.1-schnell) echo 34 ;;
    *:Qwen/Qwen-Image) echo 30 ;;
    *) echo 10 ;;
  esac
}

# 后台每 10 秒按 du 汇报下载百分比（5～PRESET_HI）；参数是本模型涉及的仓库目录（下载中的 .incomplete 文件也在里面）
progress_loop() {
  _total_kb=$(( $(model_size_gb) * 1024 * 1024 ))
  while :; do
    sleep 10
    _kb=$(du -skc "$@" 2>/dev/null | tail -n 1 | awk '{print $1}')
    _pct=$(( 5 + ${_kb:-0} * (PRESET_HI - 5) / _total_kb ))
    [ "$_pct" -gt "$PRESET_HI" ] && _pct=$PRESET_HI
    spark_status download "$_pct" "下载模型 ${MODEL}：$(( ${_kb:-0} / 1024 / 1024 ))GB / $(model_size_gb)GB"
  done
}

fetch_from() { # fetch_from <modelscope|hf> <仓库> <仓库内路径> <本地目录>：已下载完整的文件会跳过
  case "$1" in
    hf) hf download "$2" "$3" --local-dir "$4" </dev/null ;;
    *) modelscope download "$2" "$3" --local-dir "$4" </dev/null ;;
  esac
}

fetch() { # fetch <仓库> <仓库内路径> <本地目录>：按 MODEL_SOURCE 选工具
  case "${MODEL_SOURCE:-modelscope}" in
    hf-mirror) fetch_from hf "$@" ;;
    *) fetch_from modelscope "$@" ;;
  esac
}

# 把已下载的预置模型文件软链进数据盘的 ComfyUI 模型目录（extra_model_paths.yaml 指向它）
link_models() {
  _base=$(source_dir)
  model_files | while read -r _repo _file _sub; do
    if [ -f "$_base/$_repo/$_file" ]; then
      mkdir -p "$CDATA/models/$_sub"
      ln -sfn "$_base/$_repo/$_file" "$CDATA/models/$_sub/"
    fi
  done
}

download() {
  _base=$(source_dir)
  _files=$(model_files)
  # 涉及的仓库目录（去重）作为进度循环的参数
  set -- $(printf '%s\n' "$_files" | awk -v b="$_base" '!seen[$1]++ {print b "/" $1}')
  spark_status download 5 "开始下载 ${MODEL}（约 $(model_size_gb)GB）"
  progress_loop "$@" &
  _pl=$!
  # 下载失败（set -e 退出）时也要停掉进度循环，否则它会在 run.sh 写 failed 之后继续写 installing
  trap 'kill "$_pl" 2>/dev/null || true' EXIT
  printf '%s\n' "$_files" | while read -r _repo _file _sub; do
    fetch "$_repo" "$_file" "$_base/$_repo"
  done
  kill "$_pl" 2>/dev/null || true
  trap - EXIT
  link_models
  if [ "$PRESET_HI" -lt 75 ]; then
    spark_status download "$PRESET_HI" "预置模型就绪，接着安装工作流"
  else
    spark_status install 78 "模型就绪"
  fi
}

# ---------- 工作流（部署包 workflows/manifest.json，计划 11） ----------

# 清单展开成行（模型按目标 <dir>/<name> 去重、节点按 name 去重），每个字段再按平台的规则校验一遍——后面的 sh 按空白切分、拼进路径：
#   model <name> <dir> <size_gb> <sha256|-> <ms_repo|-> <ms_file|-> <hf_repo|-> <hf_file|->
#   node <name> <git> <commit>
#   wf <slug> <file>
wf_lines() {
  python3 -I - "$MANIFEST" "$MODEL_DIRS" <<'PY'
import json, re, sys

def fail(msg):
    sys.stderr.write("workflows/manifest.json: " + msg + "\n")
    sys.exit(1)

SEG = r"[A-Za-z0-9][A-Za-z0-9._-]*"
SLUG = re.compile(r"[a-z][a-z0-9-]{1,47}\Z")
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")
NODE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}\Z")
REPO = re.compile(SEG + "/" + SEG + r"\Z")
FILE = re.compile(SEG + "(/" + SEG + r")*\Z")
GIT = re.compile(r"https://(github\.com|gitee\.com|gitcode\.com)/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\Z")
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
DIRS = sys.argv[2].split()

def text(v):
    return v if isinstance(v, str) else ""

try:
    with open(sys.argv[1]) as f:
        doc = json.load(f)
except (OSError, ValueError) as e:
    fail("读取失败：%s" % e)
def items(v, what):
    # 列表且每项都是对象；null / 缺省 = 空
    if v is None:
        return []
    if not isinstance(v, list) or not all(isinstance(x, dict) for x in v):
        fail(what + " 格式不对（应是对象数组）")
    return v

if not isinstance(doc, dict):
    fail("格式不对（应是对象）")

models, nodes, out_m, out_n, out_w = set(), {}, [], [], []
for w in items(doc.get("workflows"), "workflows"):
    slug, file = text(w.get("slug")), text(w.get("file"))
    if not SLUG.match(slug):
        fail("工作流标识 %r 不合法" % slug)
    if file != slug + ".json":
        fail("工作流 %s 的文件名 %r 不合法" % (slug, file))
    out_w.append("wf %s %s" % (slug, file))
    for m in items(w.get("models"), "工作流 %s 的 models" % slug):
        name, d = text(m.get("name")), text(m.get("dir"))
        if not NAME.match(name) or d not in DIRS:
            fail("工作流 %s 的模型 %r（目录 %r）不合法" % (slug, name, d))
        if d + "/" + name in models:
            continue
        models.add(d + "/" + name)
        size = m.get("size_gb")
        if isinstance(size, bool) or not isinstance(size, (int, float)) or not 0 < size <= 200:
            fail("模型 %s 的大小 %r 不合法" % (name, size))
        sha = text(m.get("sha256")).lower()
        if sha and not HEX64.match(sha):
            fail("模型 %s 的 sha256 不合法" % name)
        src = []
        for k in ("modelscope", "hf"):
            s = m.get(k)
            if s is None:
                s = {}
            if not isinstance(s, dict):
                fail("模型 %s 的 %s 来源格式不对" % (name, k))
            repo, fl = text(s.get("repo")), text(s.get("file"))
            if not repo and not fl:
                src += ["-", "-"]
                continue
            if not REPO.match(repo) or not FILE.match(fl) or len(fl) > 256:
                fail("模型 %s 的 %s 来源 %r %r 不合法" % (name, k, repo, fl))
            src += [repo, fl]
        if src[0] == "-" and src[2] == "-":
            fail("模型 %s 没有下载来源" % name)
        out_m.append("model %s %s %s %s %s" % (name, d, "%g" % size, sha or "-", " ".join(src)))
    for n in items(w.get("nodes"), "工作流 %s 的 nodes" % slug):
        name, git, commit = text(n.get("name")), text(n.get("git")), text(n.get("commit"))
        if not NODE.match(name) or not GIT.match(git) or not HEX40.match(commit):
            fail("工作流 %s 的节点 %r 不合法" % (slug, name))
        if name in nodes:
            if nodes[name] != (git, commit):
                fail("节点 %s 在两个工作流里的 Git 地址或 commit 不同" % name)
            continue
        nodes[name] = (git, commit)
        out_n.append("node %s %s %s" % (name, git, commit))
for line in out_m + out_n + out_w:
    print(line)
PY
}

# 数据盘剩余 GiB（同 run.sh；SPARK_TEST_DF_FREE_GIB 只给单测用）；读不到时输出空串，调用方跳过预检
free_gib() {
  if [ -n "${SPARK_TEST_DF_FREE_GIB:-}" ]; then echo "$SPARK_TEST_DF_FREE_GIB"; return 0; fi
  _kb=$(df -Pk "$SPARK_DATA_DIR" 2>/dev/null | awk 'NR==2 {print $4}')
  case "$_kb" in ''|*[!0-9]*) echo "" ;; *) echo $(( _kb / 1024 / 1024 )) ;; esac
}

# disk_check <模型名> <大小 GB> <进度>：剩余 < 大小 + 5GiB → 汇报 disk: 消息（run.sh 沿用它，平台映射为错误码 disk）并退出 2
disk_check() {
  _free=$(free_gib)
  [ -n "$_free" ] || return 0
  if awk -v f="$_free" -v s="$2" 'BEGIN { exit !(f < s + 5) }'; then
    _msg="disk: 数据盘剩余 ${_free}GiB，下载模型 ${1}（约 ${2}GB）至少需要 $(awk -v s="$2" 'BEGIN { print s + 5 }')GiB"
    spark_status download "$3" "$_msg"
    printf '%s\n' "$_msg" >&2
    exit 2
  fi
}

# wf_progress <仓库目录> <大小 GB> <起> <止> <模型名> <序号> <总数>：后台每 10 秒按 du 汇报（目录里同仓库的其它文件也会算进去，封顶到「止」）
wf_progress() {
  _tkb=$(awk -v s="$2" 'BEGIN { k = int(s * 1048576); print (k > 0 ? k : 1) }')
  while :; do
    sleep 10
    _kb=$(du -sk "$1" 2>/dev/null | awk '{print $1}')
    _pct=$(( $3 + ${_kb:-0} * ($4 - $3) / _tkb ))
    [ "$_pct" -gt "$4" ] && _pct=$4
    spark_status download "$_pct" "下载模型 ${5}（${6}/${7}，约 ${2}GB）：$(( ${_kb:-0} / 1048576 ))GB"
  done
}

# wf_fetch <modelscope|hf> <仓库> <仓库内路径> <起> <止> <模型名> <序号> <总数> <大小 GB>：后台汇报进度 + 下载，返回下载工具的退出码。
# 只在 if 里调用（set -e 不生效，下载失败时由调用方决定换源还是失败）
wf_fetch() {
  _fb=$SPARK_MODELS_DIR/$1/$2
  mkdir -p "$_fb" || return 1
  wf_progress "$_fb" "$9" "$4" "$5" "$6" "$7" "$8" &
  _pl=$!
  trap 'kill "$_pl" 2>/dev/null || true' EXIT
  log "下载模型 ${6}：$1 $2 $3"
  fetch_from "$1" "$2" "$3" "$_fb"
  _rc=$?
  kill "$_pl" 2>/dev/null || true
  trap - EXIT
  return "$_rc"
}

# 模型：目标 comfyui/models/<dir>/<name> 已存在（有效软链或普通文件；失效的软链不算）→ 跳过；否则按 MODEL_SOURCE 选源（没有该源就用
# 另一个；所选来源下载失败而另一个来源存在时换它再试），预检磁盘，下载到 $SPARK_MODELS_DIR/<modelscope|hf>/<仓库>，可选 sha256 校验，
# 软链到目标。进度 WF_LO～70。
wf_models() {
  _n=$(grep -c '^model ' "$1" || true)
  _i=0
  while read -r _kind _name _dir _size _sha _msr _msf _hfr _hff <&3; do
    [ "$_kind" = model ] || continue
    _i=$((_i + 1))
    _dst=$CDATA/models/$_dir/$_name
    if [ -e "$_dst" ]; then
      log "模型 $_name 已存在，跳过"
      continue
    fi
    case "${MODEL_SOURCE:-modelscope}" in
      hf-mirror) if [ "$_hfr" != - ]; then _src=hf; else _src=modelscope; fi ;;
      *) if [ "$_msr" != - ]; then _src=modelscope; else _src=hf; fi ;;
    esac
    _lo=$(( WF_LO + (_i - 1) * (70 - WF_LO) / _n )); _hi=$(( WF_LO + _i * (70 - WF_LO) / _n ))
    spark_status download "$_lo" "下载模型 ${_name}（${_i}/${_n}，约 ${_size}GB）"
    _tries=0
    while :; do
      _tries=$((_tries + 1))
      if [ "$_src" = hf ]; then _repo=$_hfr; _file=$_hff; _alt=$_msr; else _repo=$_msr; _file=$_msf; _alt=$_hfr; fi
      _got=$SPARK_MODELS_DIR/$_src/$_repo/$_file
      # 缓存里已有这个文件（别的部署 / 预置模型下过）就不预检，下载工具会跳过完整的文件
      [ -f "$_got" ] || disk_check "$_name" "$_size" "$_lo"
      if wf_fetch "$_src" "$_repo" "$_file" "$_lo" "$_hi" "$_name" "$_i" "$_n" "$_size"; then break; fi
      if [ "$_alt" = - ] || [ "$_tries" -ge 2 ]; then die "模型 $_name 下载失败（来源 ${_src}：$_repo ${_file}）"; fi
      if [ "$_src" = hf ]; then _src=modelscope; else _src=hf; fi
      log "模型 $_name 下载失败，改用另一个来源 $_src 再试"
    done
    [ -f "$_got" ] || die "模型 $_name 下载后找不到文件 $_got"
    if [ "$_sha" != - ]; then
      spark_status download "$_hi" "校验 sha256：${_name}"
      _sum=$(sha256sum "$_got" | awk '{print $1}')
      if [ "$_sum" != "$_sha" ]; then
        rm -f "$_got"
        die "模型 $_name 的 sha256 校验失败（期望 ${_sha}，实际 ${_sum}），已删除下载的文件，请重试"
      fi
    fi
    mkdir -p "$CDATA/models/$_dir"
    ln -sfn "$_got" "$_dst"
  done 3<"$1"
}

# node_fetch_from <地址> <临时目录> <commit> <mirror|direct>：clone → checkout → 子模块（mirror 时子模块的 GitHub 地址也经镜像）。
# 失败返回非零（调用方在 if 里，set -e 不生效，每一步自己判断）
node_fetch_from() {
  git clone --quiet --no-checkout "$1" "$2" </dev/null || return 1
  git -C "$2" checkout --quiet "$3" </dev/null || return 1
  # 有的节点带子模块（如 ComfyUI_UltimateSDUpscale 的 repositories/ultimate_sd_upscale）；子模块的地址与 commit 由节点仓库的
  # gitlink 固定（不做主机白名单检查，见 deploy/apps/README.md）
  if [ -f "$2/.gitmodules" ]; then
    if [ "$4" = mirror ]; then
      git -C "$2" -c "url.${SPARK_GIT_MIRROR:-}https://github.com/.insteadOf=https://github.com/" submodule update --init --recursive </dev/null || return 1
    else
      git -C "$2" submodule update --init --recursive </dev/null || return 1
    fi
  fi
  return 0
}

# node_fetch <git> <临时目录> <commit>：GitHub 地址且配置了 SPARK_GIT_MIRROR 时先整套经镜像取（clone + checkout + 子模块），
# 任一步失败就清掉临时目录、整套直连重来
node_fetch() {
  case "$1" in
    https://github.com/*)
      if [ -n "${SPARK_GIT_MIRROR:-}" ]; then
        node_fetch_from "${SPARK_GIT_MIRROR:-}$1" "$2" "$3" mirror && return 0
        log "经镜像获取 $1 失败，改为直连"
        rm -rf "$2"
      fi ;;
  esac
  node_fetch_from "$1" "$2" "$3" direct
}

# pip 约束：镜像自带的 PIP_CONSTRAINT（NVIDIA 镜像用它钉住自家编译的 torch 等）+ 当前已装的 torch / CUDA 相关包的版本，
# 节点的 requirements.txt 不能把它们换掉（换成 PyPI 的通用版会让 GPU 不可用）
pip_constraints() { # pip_constraints <输出文件>
  : > "$1"
  for _pc in ${PIP_CONSTRAINT:-}; do
    if [ -f "$_pc" ]; then cat "$_pc" >> "$1"; printf '\n' >> "$1"; fi
  done
  python3 -m pip list --format=freeze 2>/dev/null |
    grep -iE '^(torch|torchvision|torchaudio|torchsde|triton|numpy|transformers|safetensors|xformers|flash[-_]attn|cuda-python|nvidia-[a-z0-9-]+)==' >> "$1" || true
}

node_pip() { # node_pip <节点目录>：有 requirements.txt 才装；失败返回非零
  [ -f "$1/requirements.txt" ] || return 0
  _c=$SPARK_APP_DIR/.pip-constraints.txt
  pip_constraints "$_c"
  python3 -m pip install -c "$_c" -r "$1/requirements.txt" </dev/null
}

# 节点：custom_nodes/<name>/.spark-commit = commit → 跳过；否则取到 comfyui/.tmp-nodes/<name>（不在 custom_nodes 里，ComfyUI 不会
# 把半成品当节点加载）、装依赖，成功后替换旧目录并写 .spark-commit。进度 70～78。清单里不再有的节点不删（D8）。
wf_nodes() {
  # git 不等终端输入密码（私有 / 不存在的仓库直接失败）；传输 60 秒低于 1KB/s 视为卡死，失败后走直连
  export GIT_TERMINAL_PROMPT=0 GIT_HTTP_LOW_SPEED_LIMIT=1000 GIT_HTTP_LOW_SPEED_TIME=60
  _m=$(grep -c '^node ' "$1" || true)
  _j=0
  # 上次中断留下的临时目录（含旧版放在 custom_nodes/.tmp-* 的）
  rm -rf "$CDATA/.tmp-nodes"
  for _old in "$CDATA"/custom_nodes/.tmp-*; do
    if [ -e "$_old" ]; then rm -rf "$_old"; fi
  done
  while read -r _kind _name _git _commit <&3; do
    [ "$_kind" = node ] || continue
    _j=$((_j + 1))
    _ndir=$CDATA/custom_nodes/$_name
    if [ "$(cat "$_ndir/.spark-commit" 2>/dev/null || true)" = "$_commit" ]; then
      log "节点 $_name 已是 ${_commit}，跳过"
      continue
    fi
    spark_status install "$(( 70 + (_j - 1) * 8 / _m ))" "安装节点 ${_name}（${_j}/${_m}）"
    _tmp=$CDATA/.tmp-nodes/$_name
    rm -rf "$_tmp"
    mkdir -p "$CDATA/.tmp-nodes"
    if ! node_fetch "$_git" "$_tmp" "$_commit"; then
      rm -rf "$_tmp"
      die "节点 $_name 获取失败（$_git @ ${_commit}），见上面的输出"
    fi
    if ! node_pip "$_tmp"; then
      rm -rf "$_tmp"
      die "节点 $_name 的依赖（requirements.txt）安装失败，见上面的输出"
    fi
    rm -rf "$_ndir"
    mv "$_tmp" "$_ndir"
    printf '%s\n' "$_commit" > "$_ndir/.spark-commit"
  done 3<"$1"
  rm -rf "$CDATA/.tmp-nodes"
}

# 工作流文件：每个 wf 复制为 user/default/workflows/spark-<slug>.json；目录里不在清单中的 spark-*.json 删除（用户自己的文件不动）。
# 参数是 wf_lines 的输出文件（不存在 = 清单为空）。结果个数放 WF_COUNT。
wf_files() {
  mkdir -p "$WFDIR"
  _keep=" "
  WF_COUNT=0
  if [ -f "$1" ]; then
    while read -r _kind _slug _file <&3; do
      [ "$_kind" = wf ] || continue
      cp "$SPARK_APP_DIR/workflows/$_file" "$WFDIR/.spark-$_slug.json.tmp"
      mv -f "$WFDIR/.spark-$_slug.json.tmp" "$WFDIR/spark-$_slug.json"
      _keep="$_keep$_slug "
      WF_COUNT=$((WF_COUNT + 1))
    done 3<"$1"
  fi
  for _f in "$WFDIR"/spark-*.json; do
    [ -e "$_f" ] || continue
    _s=${_f##*/}; _s=${_s#spark-}; _s=${_s%.json}
    case "$_keep" in
      *" $_s "*) ;;
      *) rm -f "$_f"; log "移除工作流文件 ${_f##*/}" ;;
    esac
  done
}

wf_install() {
  _lines=$SPARK_APP_DIR/.wf-lines
  rm -f "$_lines"
  if [ -f "$MANIFEST" ]; then
    wf_lines > "$_lines"
    wf_models "$_lines"
    wf_nodes "$_lines"
  fi
  wf_files "$_lines"
  rm -f "$_lines"
  [ -f "$MANIFEST" ] && spark_status install 79 "工作流就绪：${WF_COUNT} 个"
  return 0
}

# extra_model_paths.yaml：数据盘上的模型目录与自定义节点目录。unet / clip 是 ComfyUI 的旧名，并进 diffusion_models / text_encoders
# （同一键下多行路径）；is_default 让下载 / 保存类节点默认落到数据盘
write_yaml() {
  _y=$SPARK_APP_DIR/extra_model_paths.yaml
  {
    printf '# 由 app.sh start 生成（每次启动覆盖）：模型、自定义节点放在数据盘\n'
    printf 'spark:\n'
    printf "  base_path: '%s/'\n" "$CDATA"
    printf '  is_default: true\n'
    for _d in $MODEL_DIRS; do
      case "$_d" in
        unet|clip) ;;
        diffusion_models) printf '  diffusion_models: |\n    models/diffusion_models/\n    models/unet/\n' ;;
        text_encoders) printf '  text_encoders: |\n    models/text_encoders/\n    models/clip/\n' ;;
        *) printf '  %s: models/%s/\n' "$_d" "$_d" ;;
      esac
    done
    printf '  custom_nodes: custom_nodes/\n'
  } > "$_y.tmp"
  mv -f "$_y.tmp" "$_y"
}

case "${1:-}" in
  install)
    ensure_dirs
    if [ -f "$MANIFEST" ]; then PRESET_HI=35; fi
    if [ "$MODEL" != none ]; then
      download
      WF_LO=$PRESET_HI
    fi
    wf_install
    ;;
  start)
    ensure_dirs
    write_yaml
    if [ "$MODEL" != none ]; then link_models; fi
    # ComfyUI 自带的示例输入图（默认工作流的 LoadImage 用它）：输入目录换到数据盘后拷一份，用户已有的不覆盖
    if [ -f "$COMFY/input/example.png" ] && [ ! -e "$CDATA/input/example.png" ]; then
      cp "$COMFY/input/example.png" "$CDATA/input/" 2>/dev/null || true
    fi
    # 1.0.0 的用户目录在镜像层（/opt/ComfyUI/user，容器重建会丢）：第一次换到数据盘用户目录时拷过来一次，之后不再拷
    if [ ! -e "$CDATA/.spark-user-migrated" ]; then
      if [ -d "$COMFY/user" ]; then cp -Rn "$COMFY/user/." "$CDATA/user/" 2>/dev/null || true; fi
      : > "$CDATA/.spark-user-migrated"
    fi
    # 每次启动换新的 comfyui.log（上一次的留一份 comfyui.log.1）
    if [ -f "$SPARK_APP_DIR/comfyui.log" ]; then mv -f "$SPARK_APP_DIR/comfyui.log" "$SPARK_APP_DIR/comfyui.log.1"; fi
    cd "$COMFY"
    python3 main.py --listen 127.0.0.1 --port "$SPARK_PORT" --extra-model-paths-config "$SPARK_APP_DIR/extra_model_paths.yaml" \
      --user-directory "$CDATA/user" --input-directory "$CDATA/input" --output-directory "$OUTDIR" \
      </dev/null >>"$SPARK_APP_DIR/comfyui.log" 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    echo "comfyui started, pid $(cat "$SPARK_APP_DIR/service.pid"); log: $SPARK_APP_DIR/comfyui.log"
    ;;
esac
