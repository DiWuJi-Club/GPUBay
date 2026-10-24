# 工作流目录（ComfyUI 工作流市场，计划 11）

每个子目录是一个 ComfyUI 工作流，目录名就是 slug（`^[a-z][a-z0-9-]{1,47}$`，创建后不可改；容器内安装为
`/root/data/comfyui/user/default/workflows/spark-<slug>.json`）。目录里只能有下面这些文件：

| 文件 | 必需 | 内容 |
|---|---|---|
| `workflow.json` | 是 | 工作流信息 + 本次导入的版本号、更新说明（字段见下表） |
| `graph.json` | 是 | ComfyUI 前端保存的 **UI 格式**工作流（`nodes` / `links` / `widgets_values`，≤ 256KB），原样装进容器，用户在 ComfyUI 里打开的就是它 |
| `api.json` | 否 | 同一工作流的 **API 格式**（「导出 (API)」，≤ 256KB），快速出图提交的就是它；没有它 = 不支持快速出图（`manifest.inputs / outputs` 必须为空） |
| `manifest.json` | 是 | 模型、自定义节点、快速出图参数、输出节点（规则见设计文档 §3.2，`comfywf.ValidateManifest`） |
| `cover.png` / `cover.jpg`（或 `cover.jpeg`）/ `cover.webp` | 否 | 市场卡片封面，≤ 2MB；建议 512×288（16:9） |

`workflow.json` 字段：

| 字段 | 说明 |
|---|---|
| `name` | 名称，1～48 个字符 |
| `summary` | 一句话简介（卡片副标题），≤ 120 个字符 |
| `description` | 详细说明，≤ 8000 个字符，可用换行与 `- ` 列表（同应用的 description） |
| `category` | `txt2img`（文生图）/ `img2img`（图生图）/ `video`（视频）/ `upscale`（放大修复）/ `other` |
| `tags` | 标签，≤ 8 个，每个 1～16 个字符 |
| `sort` | 市场页排序，小的在前 |
| `version` | 版本号（如 `1.0.0`，`^[0-9A-Za-z][0-9A-Za-z.-]{0,31}$`）；同一工作流重复导入同一版本号会报错，改了文件请升版本 |
| `changelog` | 本版本的更新说明，≤ 1000 个字符 |
| `comfyui_min` | 需要的最低 ComfyUI 版本（如 `0.3.60`），只做展示；导入时合并进 manifest 的 `comfyui_min` |

## 导入

```bash
platform admin workflow import deploy/workflows
```

每个目录导入为 draft 工作流（slug 不存在时新建，并设封面）+ draft 版本，输出 `slug version created|version-added`；
导入不发布，到后台逐个检查后发布（super + TOTP）。

## 首批工作流（规格 §9，D12）

全部带 `api.json`（支持快速出图），基于 ComfyUI v0.3.60 自带的官方模板（`comfyui-workflow-templates==0.1.86`，
即 [Comfy-Org/workflow_templates](https://github.com/Comfy-Org/workflow_templates) tag `v0.1.86`）。大小为十进制 GB（字节 / 10⁹，两位小数）。

| slug | 名称 | 分类 | 模型 → 目录 | 下载 | 节点 | 快速出图参数 | 来源模板 |
|---|---|---|---|---|---|---|---|
| `sdxl-txt2img` | SDXL 文生图 | txt2img | `sd_xl_base_1.0.safetensors` → checkpoints | 6.94 GB | — | prompt、negative、width、height（512～2048 步长 64）、steps、cfg、sampler、seed | `default.json` |
| `sdxl-img2img` | SDXL 图生图 | img2img | 同上（共用） | 6.94 GB | — | image、prompt、negative、denoise（0.2～1，默认 0.75）、steps、cfg、seed | `image2image.json` |
| `flux-schnell-txt2img` | FLUX.1-schnell 文生图（fp8） | txt2img | `flux1-schnell-fp8.safetensors` → checkpoints | 17.24 GB | — | prompt、width、height、steps（1～8，默认 4）、seed | `flux_schnell.json` |
| `qwen-image-txt2img` | Qwen-Image 文生图 | txt2img | `qwen_image_fp8_e4m3fn` → diffusion_models、`qwen_2.5_vl_7b_fp8_scaled` → text_encoders、`qwen_image_vae` → vae | 30.06 GB | — | prompt、negative、width、height（默认 1328，步长 16）、steps、cfg、seed | `image_qwen_image.json` |
| `wan22-5b-ti2v` | Wan2.2 5B 文 / 图生视频 | video | `wan2.2_ti2v_5B_fp16` → diffusion_models、`umt5_xxl_fp8_e4m3fn_scaled` → text_encoders、`wan2.2_vae` → vae | 18.15 GB | — | prompt、negative、width（320～1280，默认 1280）、height（320～1280，默认 704，步长 32）、length（帧数 1～121，步长 4，默认 121）、steps（20）、seed | `video_wan2_2_5B_ti2v.json` |
| `sdxl-ultimate-upscale` | SDXL 放大修复（2×） | upscale | SDXL base（共用）+ `RealESRGAN_x4plus.safetensors` → upscale_models | 7.01 GB | `ComfyUI_UltimateSDUpscale` | image、prompt、denoise（默认 0.3）、upscale_by（1～4，默认 2）、seed | 节点仓库自带的 `example_workflows/basic-usdu.json` |

六个全装在同一个 ComfyUI 上时，SDXL base 只下一次（按 `<dir>/<name>` 去重），合计约 72.46 GB。

### 与官方模板的差异（负责人裁定，2026-10-24）

- **sdxl-txt2img / sdxl-img2img**：官方 `default` / `image2image` 模板是 SD1.5（`v1-5-pruned-emaonly-fp16`），改成 SDXL base
  `sd_xl_base_1.0.safetensors`；文生图 `EmptyLatentImage` 512×512 → 1024×1024；图生图 `ImageScaleToTotalPixels`
  `megapixels` 0.25 → 1.0（缩放方式 `nearest-exact` → `lanczos`）；KSampler 改为 25 步、CFG 7（图生图 denoise 0.75）；
  模板里旧式的 `control_after_generate = true` 规范成 `"randomize"`；改写了说明 SD1.5 / 下载地址的 MarkdownNote。
- **qwen-image-txt2img**：删掉模板里旁路（`mode: 4`）的 `LoraLoaderModelOnly`（Lightning 8 步 LoRA，id 73）及其分组，
  `UNETLoader 37 → ModelSamplingAuraFlow 66` 直连；删掉 RTX 4090 显存对照表（id 69），参数表去掉 LoRA / distill 行；
  `shift` 3.1000000000000005 → 3.1；清掉 `extra` 里作者插件留下的 `VHS_*` / `ue_links`。默认 1328×1328，步长 16。
- **wan22-5b-ti2v**：快速出图**只做文生视频**——模板的 `LoadImage`（id 56）默认旁路，「导出 (API)」时会被丢掉，
  `manifest` 无法表达「可选图片」，所以没有 `image` 参数；graph 里保留旁路的 `LoadImage`，用户在 ComfyUI 里 Ctrl + B
  启用即可图生视频。默认 1280×704、121 帧、20 步；快速出图宽高上限 1280、帧数上限 121（模板默认就是 5B 模型的推荐上限）。`umt5` 的 `properties.models` 地址从 Wan 2.1 仓库改成 Wan 2.2 仓库（同一文件）。
- **sdxl-ultimate-upscale**：放大模型用 `RealESRGAN_x4plus.safetensors`（BSD-3-Clause，Comfy-Org 官方重新打包的 safetensors），
  **不用**规格里的 `4x-UltraSharp.pth`（CC BY-NC-SA 4.0，非商用许可，不能在付费平台上分发）。图由节点自带示例改成：
  `LoadImage → UltimateSDUpscale（SDXL checkpoint、正 / 反 CLIPTextEncode、UpscaleModelLoader）→ SaveImage`；
  分块 1024×1024（适合 SDXL），20 步、CFG 7、euler / normal、denoise 0.3、Linear、无接缝修复。
  `node_types` 写全四个类型（`UltimateSDUpscale`、`UltimateSDUpscaleNoUpscale`、`UltimateSDUpscaleCustomSample`、`UltimateSDUpscaleGuider`）。
- 全部模型都给 `sha256`、同时给 `modelscope` 与 `hf` 来源；`hf` 用仓库的**现名**（Comfy-Org 有几个仓库改过名，旧名靠 307 跳转，
  不依赖 hf-mirror 是否跟随跳转）。
- 各 graph 的 `properties.models`（ComfyUI 前端「缺模型」对话框用）改成清单里的文件名与 huggingface.co 原始地址；
  下载地址类的 MarkdownNote 改成「模型已由平台安装到 `/root/data/comfyui/models/…`」。
- 参数默认值都等于 `api.json` 里的现值（ComfyUI 界面里直接运行与快速出图默认出同样的结果）；种子不设默认（平台随机，D11），图片必须上传。
- 封面是占位图（PIL 生成，512×288，分类色渐变 + 名称），真机验收出图后可替换。

## 从 ComfyUI 导出两种 JSON

1. 在 ComfyUI（与镜像同版本，v0.3.60）里打开 / 搭好工作流，把每个模型加载节点的文件名改成 `manifest.models[].name`
   （D16：graph 与 api 里引用的模型文件名必须都在清单里，目录要与加载节点匹配：`CheckpointLoaderSimple` → checkpoints、
   `UNETLoader` → diffusion_models / unet、`VAELoader` → vae、`CLIPLoader` 等 → text_encoders / clip、`UpscaleModelLoader` → upscale_models、`LoraLoader` → loras）。
2. 删掉不用的旁路 / 静音节点（尤其是引用了清单外模型的），别让用户打开时看到缺模型。
3. 菜单「Workflow → Export」保存 UI 格式 → `graph.json`；「Workflow → Export (API)」→ `api.json`（API 格式里没有旁路节点和
   Note / MarkdownNote / Reroute / PrimitiveNode；种子的 `control_after_generate` 与 LoadImage 的上传按钮只在 UI 格式里）。
4. `manifest.inputs[].node / input` 指向 `api.json` 的节点 id（字符串）与输入名，且现值不能是连线（`["id", 序号]`）；
   不要把模型文件名做成参数。`outputs` 写 `SaveImage` / `SaveVideo` 等保存节点的 id。
5. 手改 graph 时保持 `links` 与各节点 `inputs[].link` / `outputs[].links` 一致（`TestWorkflowsDir` 会检查）。

## 钉自定义节点的 commit

```bash
git ls-remote https://github.com/<owner>/<repo>.git HEAD          # 40 位 commit → manifest.nodes[].commit
git clone https://github.com/<owner>/<repo>.git /tmp/n && git -C /tmp/n checkout <commit>
grep -rn NODE_CLASS_MAPPINGS /tmp/n                                # 键 → node_types（D16 按它匹配 graph 里的非内置节点）
cat /tmp/n/requirements.txt /tmp/n/.gitmodules 2>/dev/null         # pip 依赖只装 requirements.txt；注意子模块
```

Git 主机只能是 github.com / gitee.com / gitcode.com；地址必须是小写 `https://` 开头。同一个节点（`name`）在不同工作流里
必须是同一个 Git 地址与 commit（同一部署上会冲突）。

## 核实模型地址

不在开发机下载权重，只查元数据：

- ModelScope：`https://modelscope.cn/api/v1/models/<repo>/repo/files?Recursive=true` 列出文件的 `Path` / `Size` / `Sha256`；
- Hugging Face：`https://huggingface.co/api/models/<repo>/paths-info/main`（POST `paths=<file>`）看 `size` 与 `lfs.oid`（= sha256），
  或 `curl -sI https://hf-mirror.com/<repo>/resolve/main/<file>` 看状态码、`x-linked-size`、`x-linked-etag`；
- 两边 sha256 一致才写进清单；`size_gb` = 字节数 / 10⁹，保留两位小数。

## 校验

```bash
go test ./internal/comfywf/ -run TestWorkflowsDir
```

检查：六个首批目录都在、只有允许的文件；`workflow.json` 字段（未知字段报错）；`manifest.json` 过 `ValidateManifest`（含 D16
交叉校验）、`inputs` / `outputs` 非空、模型都有 sha256 与两个来源、`download_gb` > 0；`graph.json` 节点 ≥ 3 且连线自洽；
`api.json` 与 graph 的启用节点一一对应（类型、连线，且 `widgets_values` 按 `widgetNames` 表逐项等于 api 的输入值；用到表外的节点类型时要在测试里补上它的 widget 顺序）；参数默认值等于 api 现值，只填图片也能生成 prompt；封面类型与大小；
六个工作流一起生成部署包（`BundleFiles`）不冲突、同名模型的 sha256 / 大小一致。

## 核实记录（2026-10-24 Mac 元数据核实（未下载权重）；容器内实跑待验收）

在开发 Mac 上只读核实（ModelScope / Hugging Face 的 API 与 HTTP 头、`git ls-remote`、KB 级源码），没有下载任何权重、没有用 Spark。
在 Spark 容器里实际下载、安装节点、出图 / 出视频留给真机验收（计划 11 Task 16）。

**ComfyUI 与模板**：ComfyUI tag v0.3.60 = `b8730510db30c8858e1e5d8e126ef19eac395560`，`requirements.txt` 钉
`comfyui-workflow-templates==0.1.86`、`comfyui-frontend-package==1.26.13`；模板取自 workflow_templates tag `v0.1.86`
（`48d74f5badc9c7f4e3d4670c855b8ec1e7b91a27`）。各节点的输入名与顺序按 v0.3.60 源码的 `INPUT_TYPES` 核对（`api.json` 由 UI graph
按这些规则换算，未在 ComfyUI 里实际导出）。用到的节点类型都在 `internal/comfywf/builtin_nodes.go`（v0.3.60 导出）里，
UltimateSDUpscale 除外。

**模型**（ModelScope 列表 Sha256 = HF `lfs.oid` = HF `x-linked-etag`，三处一致）：

| 文件 | ModelScope | HF（现名；旧名） | 字节 | sha256 |
|---|---|---|---|---|
| `sd_xl_base_1.0.safetensors` | `AI-ModelScope/stable-diffusion-xl-base-1.0` | `stabilityai/stable-diffusion-xl-base-1.0`（不需登录） | 6,938,078,334 | `31e35c80…7e5b` |
| `flux1-schnell-fp8.safetensors` | `Comfy-Org/flux1-schnell` | `Comfy-Org/Flux-1-Schnell`；旧名 `Comfy-Org/flux1-schnell` | 17,236,328,572 | `ead42627…bf72` |
| `split_files/diffusion_models/qwen_image_fp8_e4m3fn.safetensors` | `Comfy-Org/Qwen-Image_ComfyUI` | `Comfy-Org/Qwen-Image`；旧名 `Comfy-Org/Qwen-Image_ComfyUI` | 20,430,635,136 | `98763a12…3ce3` |
| `split_files/text_encoders/qwen_2.5_vl_7b_fp8_scaled.safetensors` | 同上 | 同上 | 9,384,670,680 | `cb5636d8…c0b4` |
| `split_files/vae/qwen_image_vae.safetensors` | 同上 | 同上 | 253,806,246 | `a70580f0…3d1f` |
| `split_files/diffusion_models/wan2.2_ti2v_5B_fp16.safetensors` | `Comfy-Org/Wan_2.2_ComfyUI_Repackaged` | `Comfy-Org/Wan-2.2`；旧名 `Comfy-Org/Wan_2.2_ComfyUI_Repackaged` | 9,999,658,848 | `456f9013…ca1e` |
| `split_files/text_encoders/umt5_xxl_fp8_e4m3fn_scaled.safetensors` | 同上 | 同上（模板指向 Wan 2.1 仓库，同一文件） | 6,735,906,897 | `c3355d30…4f68` |
| `split_files/vae/wan2.2_vae.safetensors` | 同上 | 同上 | 1,409,400,960 | `e40321bd…d156` |
| `RealESRGAN_x4plus.safetensors` | `Comfy-Org/Real-ESRGAN_repackaged` | `Comfy-Org/Real-ESRGAN`；旧名 `Comfy-Org/Real-ESRGAN_repackaged` | 66,857,836 | `37f9a931…60bb` |

（完整 sha256 见各 `manifest.json`。）规格里 Wan2.2 写「约 17GB」，实际 18.15 GB（16.9 GiB）；Qwen-Image 合计 30.06 GB。
4x-UltraSharp 只在 ModelScope 社区镜像 `XiangZL0/4x-UltraSharp` 有（sha 与 HF `Kim2091/UltraSharp` 一致），且许可非商用，未采用。

**自定义节点**：`https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git` 默认分支是 `main`（不是 master），`HEAD` =
`a5547db9e1d07d3318bb21e9e9c474f4c1e9c8df`（2026-06-22，pyproject 1.7.2）；`NODE_CLASS_MAPPINGS` 四个键见上；没有
`requirements.txt`（只用镜像里已有的 torch / torchvision / PIL / numpy）。经 `https://gh-proxy.com/https://github.com/…` 的
`git ls-remote` 与 `--filter=blob:none` clone + checkout 在 Mac 上可用（Mac 出口在境外，机房可达性待验收）。
**注意：它有 git 子模块** `repositories/ultimate_sd_upscale` → `https://github.com/Coyote-A/ultimate-upscale-for-automatic1111`
（钉 `2322caa480535b1011a1f9c18126d85ea444f146`），算法代码在子模块里。只 clone 不 `git submodule update --init` 时，
节点首次 import 会直连 github.com 下载 master.zip，连不上就整个节点导入失败。安装脚本要在 checkout 后执行
`git submodule update --init --recursive`（同样走 Git 镜像），之后再写 `.spark-commit`。

**待真机验收（Task 16）核实**：容器内按 `MODEL_SOURCE` 两种来源实际下载并校验 sha256；hf-mirror 对改名仓库的现名可用；
节点经镜像 clone + 子模块；六个工作流在 ComfyUI 里打开无缺模型 / 缺节点；快速出图各出一张图 / 一段视频；
`LoadImage` 的默认 `example.png` 在 `--input-directory /root/data/comfyui/input` 下是否存在（不存在时 UI 里直接运行图生图 / 放大会报
`Invalid image file`，快速出图总会替换成上传的图，不受影响）。
