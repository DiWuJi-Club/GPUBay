# 计划 7 遗留事项

来源：计划 7 设计（2026-10-20）§17、实施与验收过程。均不阻塞合并。

## 上线前需要核对
- Harbor：compose 里加 goharbor 服务，建 `proxy-nvcr`（代理 nvcr.io）与 `spark` 项目；机器端 `SPARK_AGENT_ALLOW_CIDRS` 含 Harbor 地址；在 arm64 构建机上 `make app-images` 并 `docker push` 到 Harbor；后台「价格与镜像」把三个镜像的 ref 改为 Harbor 路径（替换占位条目）。
- 发布顺序：先全部 Agent（`StateReport.images`、init.sh 落镜像环境文件）后 platform；platform 先发只影响覆盖率显示「未知」。
- **存量实例要重启一次容器**才有 `/run/spark-image.environ`（新版 Agent 在 create / start 时重写 init.sh，验收缺陷 A-1）。没重启过的实例上部署应用照常进行，但不带 `image.env`，平台日志有 `appdeploy: cannot read image environment` 的 warn；依赖 CUDA 环境的应用（vLLM）会像 A-1 那样失败。上线时要么批量停启一次，要么在帮助页 / 公告里说明「部署失败时先重启实例」。
- 应用目录：vLLM 以 1.0.4 为准（工具调用、`/docs`、日志轮转，依赖 `image.env`）；Open WebUI 以 1.0.1 为准（向量模型走 ModelScope、离线启动，验收 A-5）。验收环境里的 vLLM 1.0.0 / 1.0.1 与 Open WebUI 1.0.0 已下线。生产环境首次导入直接是 vllm 1.0.4、open-webui 1.0.1、jupyterlab 1.0.0、comfyui 1.0.0。
- NGC 标签：验收用 `25.09`；上线前复核是否有更新的 arm64 + CUDA 13 版本，vLLM 的 GB10 支持以 `torch.cuda.get_device_name()` 为准。
- Open WebUI 锁在 `0.12.3`（`OPEN_WEBUI_VERSION`）：升级前在真机上复测对话与启动耗时（新版本的工具调用默认值、启动时下载的模型可能再变；1.0.1 的 `OFFLINE_MODE` 依赖 0.12.x 的环境变量名）。
- hf-mirror：对 xet 存储的仓库会 302 到 HF 的 xet CDN（国内不可达，验收 A-5 / O-12）。上线前在机房网络里实测 vLLM / ComfyUI 的「下载源：hf-mirror」选项；不可用就从 spec 里去掉该选项（发新版本），或在 `app.sh` 里对 hf-mirror 设 `HF_HUB_DISABLE_XET=1` 并实测速度。
- 默认限速 200Mbps 下 16GB 模型约 12～15 分钟，给运营做预期管理（帮助页已注明）。
- Open WebUI「首访即管理员」的提醒文案；ModelScope 当前所选模型不需要登录令牌，新增模型时核对。
- 计划 8 合并前把它的迁移改为 00021（本计划占用 00020）。

## 规格列出的遗留（本计划不做）
- 应用收费、按模型收费、Token 商城。
- 部署期间临时放开限速（需要 Agent 指令）；机器级共享模型缓存。
- 自动升级已有部署、灰度发布、版本回滚按钮（用「复制为新版本」）。
- 用户自定义应用 / 上传脚本；应用市场评分与运营数据。
- 多实例编排；应用之间的依赖声明（Open WebUI 依赖 vLLM 只靠默认地址）。
- 日志全量持久化与搜索（只保留 64KB 尾部 + 容器内文件）；Web 终端。
- 后台脚本内容审查 / shellcheck；脚本签名。
- 健康异常自动重启（只标 unhealthy / crashed）；短信通知。
- ComfyUI-Manager / 自定义节点、CivitAI、HuggingFace 热门模型搜索。
- reserved 实例开通前修改预装变量（只能卸载再添加）。
- 镜像构建流水线（CI）；Harbor 镜像 GC。

## 验收 A 的缺陷与观察（`acceptance-plan7.md`）

### 已修（2026-10-24 真机复测通过，见 `acceptance-plan7.md`「复测」）
- A-1 应用进程没有镜像 ENV：Agent init.sh 在 exec sshd 前把入口环境拷到 `/run/spark-image.environ`；Runner 部署前读取、过滤后随包下发 `image.env`，`run.sh` 先加载它再加载 `env`。没有改用 `/proc/1/environ`：PID 1 是 sshd，OpenSSH 的 setproctitle 会覆盖那块内存。
- A-2 Open WebUI 对话 400：vLLM 1.0.4 加 `--enable-auto-tool-choice --tool-call-parser hermes`；Dockerfile 锁 `open-webui==0.12.3`。
- A-3 清单与 §6.3 冲突：§16.4 第 11 项拆成「Agent 在线时停容器」与「Agent 停机（5 分钟内恢复）」两个场景，第 13 项的告警预期随之改为 `container_down` + `app_deploy_unreachable` 各一条。
- A-4「打开」指向 404 的根路径：部署视图的 `url` = 主端口 URL + `ports[0].path`；vLLM 的 `ports[0].path` 改为 `/docs`。
- O-3 vllm.log 混入上一次运行：`start` 先把旧 `vllm.log` 改名为 `vllm.log.1`。
- O-4 停机实例上的应用标签仍是绿色「运行中」：实例详情改为灰色「已停机」（与列表一致）。
- O-5 不可达计数期间 health 仍为 ok：第一次 DialError 起 running 部署 `health = unknown`。
- O-7 失败原因重复（「服务进程已退出：服务进程已退出」）：message 与错误码中文相同时只显示一次（控制台、后台实例详情）。
- A-5（复测中发现）Open WebUI 启动卡在经 hf-mirror 下载 RAG 向量模型：open-webui 1.0.1 在 install 时从 ModelScope 下载 `sentence-transformers/all-MiniLM-L6-v2`，start 用 `OFFLINE_MODE=True` 并指向本地目录（`10663d3`）；真机 25 秒 running。

### 可以继续延后
- **用户 SSH 会话仍没有镜像 ENV**（A-1 的另一半）：用户登录的 shell、`ssh host cmd` 都由 sshd 重建环境，`nvcc`、`CUDA_HOME`、`LD_LIBRARY_PATH` 不在（JupyterLab 由 `run.sh` 启动，已带上 `image.env`，其终端 / 内核也继承）。可选做法：
  - init.sh 根据 `/run/spark-image.environ` 生成 `/etc/profile.d/00-spark-image-env.sh`。需要过滤 `HOME / PWD / HOSTNAME / SHLVL` 并安全转义（用 python3 或 Agent 侧生成）。只覆盖登录 shell，不覆盖 `ssh host cmd`。
  - sshd `SetEnv`（OpenSSH ≥ 8.7）。能覆盖所有会话，但值的转义规则要处理好；一个坏值会让 sshd 起不来、整台实例连不上，影响面大。
  
  两种都要改 Agent 并重启容器，本次不做。
- **镜像 ENV 带来的行为变化**：`image.env` 让应用进程拿到镜像的全部 ENV。NGC 镜像里的 `PIP_CONSTRAINT`、`PYTHONPATH`（如有）、`LD_LIBRARY_PATH` 现在对 install / start 生效。验收 A 时 Open WebUI（独立 venv）与 JupyterLab 是在没有这些变量的情况下通过的，复测时确认它们仍正常。平台未配置 `SPARK_APP_PIP_INDEX_URL` 时 `env` 里的 `PIP_INDEX_URL=''` 会覆盖镜像里的值，可考虑空值不写进 `env`。
- A-3 的产品侧：Agent 失联导致实例暂停、Agent 恢复后重启了容器时，应用进程已不在，三个部署走 `alive=0` 变为 `failed(crashed)`，需要逐个重新部署。可考虑实例解除暂停时把 running 的部署重新排队（与续费恢复相同）。
- O-7 余项：进程在健康检查阶段退出时错误码是 `health_failed`（「健康检查未通过：服务进程已退出」）。文案通顺，但与 `crashed` 的区别对用户不明显，可考虑统一为「启动后服务进程退出」。
- O-1：本地 tag 镜像每次登记 / 发布都广播 `pull_image`，Docker Hub 不可达导致指令记录一片红。可考虑机器已缓存该 ref 时不下发 pull。
- O-2：平台日志没有部署生命周期的 INFO 行（只有 unreachable 的 WARN），排障依赖事件表与后台页面。
- O-6：容器内 `vllm.log` / `log` 里有 `api_key` 原文（vLLM 自己打印的参数），只在用户容器内；帮助页可以提醒「分享数据盘内容前注意密钥」。
- O-8：换版本后的重新部署会重跑 install，`modelscope download` 对已存在文件秒过，符合预期。
- O-9：开发 Mac 休眠导致的 `upstream keepalive failed` / `machine_offline`，环境因素。
- O-10：释放实例会洗掉数据盘，模型缓存随之清空；Task 23 的 ComfyUI 需要在新实例上重新下载。
- O-11：达到上限后「添加应用」按钮隐藏（不是置灰），API 上 `app_limit` 先于 `app_exists`。两者都能达到提示目的。
- O-12：hf-mirror 的 xet 重定向（见「上线前需要核对」）。
- O-13：由 open-webui 1.0.0 升级到 1.0.1 的部署，Open WebUI 数据库里持久化的 `RAG_EMBEDDING_MODEL` 仍是 HF 模型名，离线模式下知识库不可用（启动、对话不受影响），需在其「管理员设置 → 文档」改为本地路径或卸载重装。生产没有 1.0.0 部署，不受影响。
- O-14：重新部署接口带 `env` 时是整表语义（没给的普通变量取默认值，secret 未给则沿用）。控制台总是带当前值；接口文档 / 帮助页可写明。
- O-15：`image.env` 里有 `NVIDIA_VISIBLE_DEVICES=void`（CDI 注入 GPU 时入口进程的环境），对应用无影响，仅记录。
- `/run/spark-image.environ` 在容器内是 0644（root 所有，容器只有 root 用户），Runner 生成的 `image.env` 是 0600。如果以后镜像里有非 root 用户，可改为 0600。
- vLLM 的 `app.sh` 里有 `$MODEL_ID：` 这类变量名紧跟全角标点的写法：dash（容器的 `/bin/sh`）正确，但在 bash 的某些 locale 下会把多字节字符当作变量名的一部分（open-webui 1.0.1 已改为 `${…}`）。下次改 vLLM 脚本时一并加花括号。

## 实施中记录的可以继续延后
- 新 Agent 本机没有任何镜像时 `StateReport.images` 为空列表，在线上与「未上报」不可区分，后台显示「未知」；机器刚装好的窗口极短。
- `stopped` 的部署不在 Runner 的候选查询里（`next_poll_at` 推到 24 小时后），实例恢复 running 时由钩子重排；规格 §5.3 表里的 `stopped` 行等价。
- Runner 的咨询锁按轮取放（与清扫器一致），不是会话长持；多副本交替工作，效果相同。
- 本机没有 `setsid` 的开发 Mac 用 `python3 -c os.setsid` 脱离会话；镜像规范要求 `util-linux`，生产只走 `setsid`。
- 部署包上传用 `tar -x -f -`，依赖镜像里的 `tar`（规范已要求）；没有 `tar` 的镜像上传失败会显示 `upload_failed`。
- 后台版本编辑页的脚本框是 `<textarea>`，没有语法高亮。
- 覆盖率的「未知」= 5 分钟内没上报；Agent 60 秒一报，正常机器不会落入。
- `apps_summary` 让实例列表每次多一次查询（一次 `ANY($1)`，不是 N+1）。

## 实施中确定的产品行为（裁定）
- 应用端口由平台登记（不经 `rental.AddPort`），卸载时以用户身份审计 `instance.port_delete`。
- 卸载时实例停机：立即 `removed` + `cleanup_pending`，实例恢复后由 Runner 删目录。
- 重新部署换版本时旧端口行先删，新版本 running 时重新登记。
- `platform admin app import` 的镜像按 `images.name` 匹配；只导入为 draft，发布在后台做。
- 镜像环境读不到时照常部署（不带 `image.env`，记 warn），不让所有部署失败；连接层错误按 unreachable 计数。
- 部署的 `url` 是「打开」的地址（主端口 URL + `ports[0].path`），`links` 仍相对主端口根。
- 应用启动不得依赖境外网络：启动阶段要用的模型在 install 时从 ModelScope 下载到 `/root/data/models`，start 尽量离线（Open WebUI 1.0.1 起）。
（实施中新增的裁定按实际补充。）
