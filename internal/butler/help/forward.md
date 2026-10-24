# 端口转发与文件传输

- `scp` / `sftp` / `rsync` 都可用（走同一条 SSH 连接）；大文件请放到 `/root/data`。
- 本地转发 `ssh -L 8888:127.0.0.1:8888 ins-xxxxxxxx@…` 可在本机浏览器访问容器里的服务；`-R`、`-A`（agent 转发）也支持；不支持 X11。
- VS Code Remote-SSH：Host 填 `ssh.您的域名`，User 填实例 ID。
