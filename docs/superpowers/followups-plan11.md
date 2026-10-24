# 计划 11 遗留事项

来源：计划 11 设计（2026-10-24）§17、实施期间的裁定记录、真机验收（`acceptance-plan11.md`）。均不阻塞合并。

## 上线前核对

- **Git 镜像可达性**：`SPARK_APP_GIT_MIRROR` 默认 `https://gh-proxy.com/`。
  - 验收时在 221 的实例容器里，经它 clone `ComfyUI_UltimateSDUpscale`，再 `submodule update` 取到 `Coyote-A/ultimate-upscale-for-automatic1111@2322caa`，安装节点一共约 10 秒。
  - 上线前要在机房出口再测一次。gh-proxy.com 是公共服务，随时可能限速或下线，不稳定时换成 `https://ghfast.top/` 等，或自建镜像（内网地址的网段加进 `SPARK_AGENT_ALLOW_CIDRS`）。改完变量，用户重新部署即可生效。
  - 镜像失效时 `app.sh` 会回退直连 github.com。直连在机房大概率不通，节点安装会失败（`script_failed`，日志里有 git 的报错）。
- **六个工作流在生产镜像上各跑一遍**：验收在 `spark/comfyui:25.09`（本地 tag）上把六个都跑通了。
  - SDXL 文生图 / 图生图 / 放大修复、FLUX、Qwen-Image 各出一张图，Wan2.2 出一段 5 秒视频；ComfyUI 界面里加载侧栏工作流也能直接运行。
  - 生产镜像改从 Harbor 拉取，或 NGC 标签变化之后，要再跑一遍。
- **下载时间**：验收环境 ModelScope 的实际速度约 22～25MB/s，`SPARK_AGENT_BANDWIDTH_MBPS=1000`，瓶颈不在限速。实测：

  | 工作流 | 下载量 | 用时 |
  |---|---|---|
  | SDXL | 6.94GB | 约 5 分钟 |
  | FLUX.1-schnell fp8 | 17.24GB | 约 13 分钟 |
  | Wan2.2 5B | 18.15GB | 约 12.5 分钟 |
  | Qwen-Image | 30.06GB | 约 21 分钟 |

  - 默认限速 200Mbps（25MB/s）下用时与上表相当，所以 FLUX / Wan 首装约 12～15 分钟，Qwen 约 20 分钟。
  - 规格 §16.4 第 5 项写的「FLUX ≤ 10 分钟」在这种速度下做不到。帮助页与市场卡片的「需下载约 X GB」之外，应再给一个时间预期（例如按 20MB/s 估算）。
- **封面版权**：六张封面是 PIL 生成的占位图（分类色渐变 + 名称）。上线前换成用自己的模型生成的图，例如验收中各工作流的快速出图输出，不要用网上找的图。
- **`install_key` 兼容（Task 3）**：
  - 计划 11 的 `run.sh` 把 `install_key` 改成三段（`SPARK_SPEC_HASH` + `cksum(env)` + `cksum(workflows/manifest.json)`，后者没有时为空串）。与存量 `.installed`（两段式）比较时忽略尾部空白，所以计划 7 的存量部署升级平台后续费或停机恢复不会重跑 install。
  - 一旦装了工作流（或换到 1.1.0），第三段就不再为空，会正常重跑 install，已下载的模型和已 clone 的节点都秒过。
  - 真机第 8 项验证了集合不变时的恢复是 `skip install`。
- **comfyui 应用版本**：
  - 验收前半段（第 1～7 项）部署的是 `c0f4a21` 的 1.1.0 `app.sh`。
  - 后半段（第 8～10 项）另以验收专用版本号 `1.1.0-head` 导入了集成分支 `10dbe9b` 的 `app.sh`（多了「旧位置同名模型软链复用」与 Git 路径正则对齐）。生产首次导入的就是这份 1.1.0。
  - 1.0.0 → 1.1.0 的升级路径（旧 `/opt/ComfyUI/models` 软链复用）只有 sh 单测，真机没有测。
- **迁移顺序**：集成分支已开启 goose `WithAllowOutofOrder`。验收库 `spark_p11` 先有 00024、后补 00021，没有报错。生产按编号顺序发布即可。
- **公共服务依赖**：`hf-mirror` 对 Comfy-Org 改名仓库会 302 到 HF 的 xet CDN（计划 7 的 O-12）。验收全程用默认的 modelscope，没有实测 `MODEL_SOURCE = hf-mirror`。上线前在机房实测一次，不可用就在帮助页注明「工作流只支持 modelscope」。

## 实施期间的裁定与遗留（`p11-decisions` 中标 followup 的条目）

- **goose 乱序**：00024 先于 00021～00023 落库时，旧代码的 `db.Migrate` 会失败。集成分支已加 `WithAllowOutofOrder`（见上）。计划 9 / 10 合并时核对迁移编号不冲突。
- **规格 §14 的不变式**：「installed 行只出现在 running / stopped 的部署上」做不到。添加工作流会把部署重新排队，而已有行仍是 installed。集成测试实际检查的是：
  - running 部署没有 pending / failed 行；
  - installed 行都有 `installed_at`；
  - 每部署 ≤ 8 行；
  - 只出现在 comfyui 部署上；
  - 版本属于该工作流。

  需要同步改规格 §14。
- **节点的 pip 依赖装在镜像层**（`/usr/local/lib/python3.12/dist-packages`），`.spark-commit` 却在数据盘。容器重建（换机之外的重建，例如镜像升级）后，节点目录还在，`.spark-commit` 也匹配，install 会跳过 pip，节点可能因缺依赖导入失败。首批唯一的节点（UltimateSDUpscale）没有 `requirements.txt`，不受影响。新增带依赖的节点前要解决：例如把依赖装进数据盘的 venv / `--target`，或在 `.spark-commit` 里带上镜像 ID。
- **用户改过的 `spark-*.json` 会被覆盖**：平台放进 `user/default/workflows/` 的 `spark-<slug>.json` 每次 install 都会重写。用户在 ComfyUI 里直接改了再「保存」，下次重新部署就会丢。帮助页已写明「另存为其它名字」。验收第 7 项看到六个文件在一次更新中全部被重写（mtime 相同）。
- **上游 4x-UltraSharp 是非商用许可（D20）**：已换成 RealESRGAN_x4plus（BSD-3）。若运营想要 UltraSharp 的效果，需要另找可商用的放大模型。
- **规格 §16.4 第 3 项的写法**：「连续点 3 次运行」与抽屉的实现不一致，见下面的 O-4。改规格为「两个接口运行加一次页面运行」，或在后台 / 多标签页里验证。

## 规格列出的遗留（本计划不做）

- 用户上传 / 分享工作流；评分、评论；运营数据之外的推荐。
- ComfyUI-Manager、任意 Git 节点、CivitAI、LoRA 市场。
- 快速出图的 WebSocket 进度、取消、批量、历史页；超过一张输入图的复杂表单；参数预设保存。
- 机器级共享模型缓存与预热；按工作流计费。
- 工作流自动更新；安装时不重启 ComfyUI（热加载节点）。
- 后台可视化编辑工作流图（只接受导出的 JSON）。
- ComfyUI 版本升级（镜像里固定 v0.3.60；升级走计划 7 的镜像流程）。

## 真机验收的观察（`acceptance-plan11.md`）

（O-n 编号与验收记录一致；验收记录里的缺陷表 W-n 见该文件。）

### 缺陷（未修）
- **W-1** ComfyUI 启动日志 `Failed to initialize database`：v0.3.60 的 `--database-url` 默认是 `<程序目录>/user/comfyui.db`，1.1.0 把用户目录换到数据盘后，这个路径不存在。下一个 comfyui 版本在 `start` 里加 `--database-url "sqlite:///$CDATA/user/comfyui.db"`，补 sh 测试（启动参数快照），在真机上确认日志里没有这条错误，ComfyUI 正常建库。目前只是日志噪声。

### 已修
- **O-1** 预装应用显示「· none」/「（none）」：`f7f2cef`，真机复测通过。

### 待改进（按优先级）
- **O-11** 部署失败时 message 只有「install 失败，退出码 N」，原因只在日志里：让 `run.sh` 在 install 失败时把 stderr 最后一行（`app.sh` 的 `die` 消息）作为 message，与 D7 的 `disk:` 沿用同一处。
- **O-3** 「需下载约 X GB」没有扣除部署上已有的模型：安装弹窗与 RentDialog 按部署已装工作流的 `<dir>/<name>` 去重后再求和；平台侧 `GET …/workflows` 可以顺带返回已有模型清单。
- **O-2** 下载进度：`app.sh` 按 `size_gb` 加权、只算要下载的模型；进度消息改为「已下载 x.x / y.y GB」（本文件 `du` 减去起始值，不用整个仓库目录）。要发新的 comfyui 版本。
- **O-5** ComfyUI 重启后历史清空：重新部署之前的快速出图结果取文件会 404；没人轮询的运行被记为「任务已丢失」（统计里多出失败）。抽屉在 404 时提示「结果已保存到 /root/data/comfyui-output，可在 ComfyUI / JupyterLab 查看」；统计口径里把「丢失」与「执行失败」分开。
- **O-4** 规格 §16.4 第 3 项的「连续点 3 次运行」改为「两个接口运行 + 一次页面运行」（抽屉运行中不能再点）。
- **O-7** `platform admin app import` 遇到第一个错误就停：改为逐个继续（像工作流导入那样），或在 README 写明只拷要升级的应用目录再导入。
- **O-10** 客户端中途断开时平台记 ERROR `internal error … context canceled`：忽略 `context.Canceled`，或降为 debug。
- **O-6** `spark/comfyui:25.09` 没有 torchaudio（ComfyUI 音频节点不可用，日志里有 `ModuleNotFoundError`）：镜像重建时装上与 torch 匹配的 torchaudio。
- **O-9** 发布动态码的 5 分钟免码窗口：帮助文案里说明（运营批量发布时「有时要码、有时不要」）。
- **O-8 / O-14**：下载时间预期与 hf-mirror 未测，见「上线前核对」。
- **O-12 / O-13**：共享机器的让机记录，以及 `spark-*.json` 每次 install 都被重写，见验收记录与上面的裁定遗留。
