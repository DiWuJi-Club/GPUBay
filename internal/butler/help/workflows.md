# 工作流市场

- 在「工作流市场」挑选 ComfyUI 工作流，点「一键安装」装到您实例上的 ComfyUI；还没有 ComfyUI 时可「新开一台实例」，开通时一并预装 ComfyUI 与所选工作流（每个 ComfyUI 最多 8 个）。
- 安装 = 在实例里下载该工作流需要的模型与自定义节点，再**重启 ComfyUI**：正在生成的任务会中断；下载量见工作流卡片（「需下载约 X GB」），速度受实例出网限速影响。
- 文件位置：模型在 `/root/data/comfyui/models`，自定义节点在 `/root/data/comfyui/custom_nodes`，工作流文件在 `/root/data/comfyui/user`（ComfyUI 左侧「工作流」里以 `spark-` 开头）；一键部署时选的预置模型缓存在 `/root/data/models`（软链进 ComfyUI 模型目录）。
- 移除工作流只删工作流文件，**不删除模型与节点**（其它工作流可能也在用）；不需要时可 SSH 进去手动清理以腾出数据盘空间。
- 支持「快速出图」的工作流可在实例详情直接填参数出图；ComfyUI 的出图结果（含快速出图）保存在 `/root/data/comfyui-output`。
- 想自己装节点：SSH 进去放到 `/root/data/comfyui/custom_nodes/`，重新部署不会删除它，但平台不负责其兼容性。
