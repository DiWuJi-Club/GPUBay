# 计划 2 遗留事项

来源：计划 2 设计（2026-10-18）§9、逐任务审查与真机验收（`docs/superpowers/acceptance-plan2.md`）。均不阻塞合并。

## 上线前需要核对
- 控制面 9443 只对机房内网开放；`SPARK_AGENT_HOSTS` 填生产网关域名或 IP（证书 SAN），改动后 Agent 需重新接入（证书 SAN 不含旧地址时 Agent 连不上）。
- `SPARK_SECRET_KEY` 轮换会使 CA 不可用：轮换前备份 `platform_pki`，轮换后所有机器重新接入。
- 数据盘大小目前是全局配置 `SPARK_AGENT_DATA_SIZE_GIB`（规格写「后台按机型可改」）：按商品配置列入后续。
- 机器上 `ufw` 必须关闭（`selfcheck` 会检查）；若机房用其他防火墙，确认不会清掉 `DOCKER-USER` 跳转。
- 机器的业务网卡最好用有线 + 静态地址或 DHCP 保留；若启用 `NetworkManager-wait-online` / `systemd-networkd-wait-online`，`network-online.target` 才有意义。Agent 已能在地址就绪前推迟并重试启动（验收缺陷 B1、B2），但地址晚到期间容器不可用。
- 生产镜像需自带诊断所需工具（参考镜像不含 python、nvcc；验收只用 `nvidia-smi` 与 `libcudart` 验证 GPU）。
- `install.sh` 走内网 HTTP，二进制校验和写在脚本里，局域网中间人可同时替换脚本与校验和绕过校验：上线后改为 HTTPS，或在后台接入命令里固定脚本哈希。

## 规格列出的遗留（本计划不做）
- 机器证书续期（5 年）。
- 上游 SSH 密钥轮换（需重建全部容器）。
- 容器可写层限额（需 XFS project quota 或独立分区，要重装系统）。
- 时序监控（目前只保留最新一份 `last_metrics`）。
- Agent 自动升级（验收中升级靠重新执行接入命令或替换二进制后重启服务）。

## 可以继续延后
- 别的机器上的实例出现在本机 StateReport 里时，孤儿 wipe 以同一个实例 id 入队：期间 `HasAnyOpenCommand(id)` 会阻止原机器上对该实例的对账动作，原机器的 `beginWipe` 也会把这条孤儿 wipe 一并取消。同一实例 id 不会真的跑到两台机器上，极少出现。
- `tc tbf burst 256kbit`（规格原文）在高速率下限不到目标：burst 至少要 rate/HZ（1Gbps 需 ≥125KB，256kbit 只够约 250Mbps）；验收用 50Mbps（实测 ≈46Mbps）没问题，后续按 rate 计算 burst。
- `ensureLimit` 先 `del ingress` 再 `add`，中间有不到一秒的上行不限速窗口；可改为 `replace` 或先加新规则再删旧规则（终审 M6）。
- 同一副本上同一台机器的两个 Connect 几乎同时到达，数据库归属与内存登记可能交叉一个 tick，由每 tick 的归属检查自愈（终审 M8）。
- 指令 payload 无法转成协议消息（`toProtoCommand` 失败）时只记日志，指令保持 sent 直到清扫器判超时；可改为立即按失败处理（终审 M9）。
- `cmdrun.Exec` 在 `os.Environ()` 后追加 `LC_ALL=C`（systemd 下没问题）；`install.sh` 内嵌的 unit 与 `deploy/agent/spark-agent.service` 重复，应只保留一份来源（下载目录拷贝或 `make agent` 生成）（终审 M10）。
- Hello 只记录 `agent_version`，`last_metrics` 要等首个心跳；后台机器详情把指标时间标成 `last_seen_at`，略有误导（终审 M11）。
- 若干 fleet/app 测试丢弃了错误返回；`cmdrun.Recorder` 对未打桩命令返回 `nil, nil`，新代码路径可能空过测试，可加 Strict 模式对未打桩命令直接失败（终审 M12）。
- 清扫器的 `FailTimedOutCommands` 与网关投递用同一张 `agent_commands` 表，多副本下由咨询锁保证只有一个在判超时；指令量很大时可改为按机器分片。
- Enroll 限流按副本内存计数，多副本各自计数。
- fake Agent 的 e2e 步进等待上限 10 秒（真实时间），CI 机器很慢时需要调大 `hookWait`。
- 对账里 `unknown` 阶段不做处理；若 Agent 长时间上报 unknown（docker 挂了），只有心跳指标能看出来，可考虑加告警。
- `machine_enroll_tokens` 没有清理任务（每台机器每次接入一行，量很小）。
- 开通失败到洗机完成之间的 StateReport 会对已 failed 的实例产生一条 `orphan_container` 告警（验收观察，噪声）。
- `machine_offline` 告警在机器恢复在线后不会自动解除，需人工处理（验收观察）。
- 失联判定前的 5 分钟照常计时（剩余时长在判定失联时冻结），符合 MVP 规格「故障暂停」定义；如需按断流时刻补偿需另行设计。
- 接入令牌出现在命令行参数 / shell 历史中（一次性、24 小时有效，已接受）。
- GPU 传感器返回 `[N/A]` 时后台显示为 0°C / 0W。
- 同一毫秒内同一机器的多条指令按 UUID 排序，顺序随机（实际只在同一事务内入队时出现）；可加自增序号作次序。
- Agent 本地 `state.json` 损坏时 Agent 拒绝启动（按计划保持「宁可报错」），需要人工处理。

## 实施中确定的产品行为（裁定）
- Create 指令的 `upstream_pubkey` / `bandwidth_mbps` / `ssh_port` / `data_size_gib` 由网关在投递时填充，`rental` 的 payload 仍只有 `image_ref`。
- 孤儿容器告警用 `warning` 级别（表约束只有 warning / critical）。
- `/hooks/agent` 对未接入的机器会先接入；步进式 fake Agent 不自动心跳 / 上报。
- 网关按「每台机器的指令创建顺序」投递全部 sent 指令（设计 §3.1 优先于计划原文的「先认领再补推」）。
- Agent 所有外部命令以 `LC_ALL=C` 运行（真机 `LANG=zh_CN.utf8` 会把 `df`、错误信息本地化）。
- iptables 规则用 `iptables-restore --noflush` 一个事务重建两条链，失败时 fail closed（一个租户容器都不启动）。
- 宿主机重启恢复时挂载全部实例的数据盘（包括期望 stopped 的），避免误报 `data_disk_missing`。
- 对没有接入的机器执行「撤销接入」也会作废其未使用的接入令牌并成功返回（泄露的接入命令必须能作废）。
- Agent 启动时按 `state.json` 拉起应处于 running 的容器（重启恢复所必需），因此「Agent 停止期间人工 `docker stop`」在 Agent 启动后会被拉起，不产生 `container_down`；Agent 在线期间容器被停止才产生 `container_down`（只告警、不自动拉起、计时继续）。
- 容器被外部重启后，Agent 在 60 秒周期内补回限速（tbf + ingress police）；启动后找不到 veth 或设置限速失败时停掉容器（fail closed）。
- 容器端口绑定的地址尚未出现在本机时，Agent 不调用 `docker start`（Docker 29 在绑定失败后会丢掉容器的网络挂接），留待周期重试；检测到挂接丢失时先 `docker network connect` 再启动。
