// spark-agent 是跑在每台 DGX Spark 上的节点代理：enroll 接入、run 常驻、selfcheck 自检。
package main

import (
	"bufio"
	"context"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"spark-cloud/internal/agent/client"
	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/disk"
	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/disk/loop"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/agent/netfilter"
	netfake "spark-cloud/internal/agent/netfilter/fake"
	"spark-cloud/internal/agent/netfilter/iptables"
	"spark-cloud/internal/agent/runtime"
	"spark-cloud/internal/agent/runtime/docker"
	rtfake "spark-cloud/internal/agent/runtime/fake"
	"spark-cloud/internal/agent/state"
)

var version = "dev" // -ldflags "-X main.version=..."

const usage = `用法：
  spark-agent enroll --platform <host:port> --token <spk1_...> --ca-sha256 <指纹> [--dir /etc/spark-agent]
  spark-agent run [--dir /etc/spark-agent] [--state-dir /var/lib/spark-agent] [--fake]
  spark-agent selfcheck [--state-dir /var/lib/spark-agent]
  spark-agent version`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "spark-agent:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "version":
		fmt.Fprintf(out, "spark-agent %s\n", version)
		return nil
	case "enroll":
		return enroll(context.Background(), args[1:], out)
	case "run":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runAgent(ctx, args[1:], out)
	case "selfcheck":
		fs := flag.NewFlagSet("selfcheck", flag.ContinueOnError)
		stateDir := fs.String("state-dir", "/var/lib/spark-agent", "状态目录")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return selfcheck(context.Background(), cmdrun.Exec{}, *stateDir, out)
	}
	return errors.New(usage)
}

type agentConfig struct {
	Platform   string
	ServerName string
}

// loadAgentConfig 读 <dir>/config.yaml：只支持 `key: value` 行与 # 注释。
func loadAgentConfig(dir string) (agentConfig, error) {
	f, err := os.Open(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return agentConfig{}, err
	}
	defer f.Close()
	var cfg agentConfig
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "platform":
			cfg.Platform = v
		case "server_name":
			cfg.ServerName = v
		}
	}
	if err := sc.Err(); err != nil {
		return agentConfig{}, err
	}
	if cfg.Platform == "" {
		return agentConfig{}, errors.New("config.yaml 缺少 platform")
	}
	if cfg.ServerName == "" {
		cfg.ServerName, _, _ = net.SplitHostPort(cfg.Platform)
	}
	return cfg, nil
}

// enroll 用一次性令牌换机器证书，写入 <dir>/{agent.key,agent.crt,ca.crt,config.yaml}（目录 0700，文件 0600）。
// agent.key 已存在则复用（重新接入不换私钥）。
func enroll(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	platform := fs.String("platform", "", "平台地址 host:port")
	token := fs.String("token", "", "一次性接入令牌")
	caSHA := fs.String("ca-sha256", "", "平台 CA 证书指纹")
	dir := fs.String("dir", "/etc/spark-agent", "配置目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *platform == "" || *token == "" || *caSHA == "" {
		return errors.New("--platform、--token、--ca-sha256 都必填\n" + usage)
	}
	host, _, err := net.SplitHostPort(*platform)
	if err != nil {
		return fmt.Errorf("--platform 必须是 host:port: %w", err)
	}
	if err := ensurePrivateDir(*dir); err != nil {
		return err
	}
	key, err := client.LoadOrCreateKey(filepath.Join(*dir, "agent.key"))
	if err != nil {
		return err
	}
	certDER, caDER, err := client.Enroll(ctx, *platform, *token, *caSHA, key)
	if err != nil {
		return fmt.Errorf("接入失败: %w", err)
	}
	files := []struct {
		name string
		data []byte
	}{
		{"agent.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})},
		{"ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})},
		{"config.yaml", fmt.Appendf(nil, "platform: %s\nserver_name: %s\n", *platform, host)},
	}
	for _, f := range files {
		if err := writePrivateFile(filepath.Join(*dir, f.name), f.data); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "已接入平台 %s，证书写入 %s\n", *platform, *dir)
	return nil
}

// ensurePrivateDir 创建目录并保证权限是 0700（已存在的目录也收紧）。
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// writePrivateFile 原子写入 0600 文件：先写临时文件再改名，避免中途失败留下半个证书。
func writePrivateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // WriteFile 不改已存在文件的权限
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func runAgent(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dir := fs.String("dir", "/etc/spark-agent", "配置目录")
	stateDir := fs.String("state-dir", "/var/lib/spark-agent", "状态目录")
	fake := fs.Bool("fake", false, "用内存 fake 运行时（本地开发，不需要 Docker / root）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(out, nil))
	cfg, err := loadAgentConfig(*dir)
	if err != nil {
		return fmt.Errorf("读取配置失败（先运行 spark-agent enroll）: %w", err)
	}
	keyPath := filepath.Join(*dir, "agent.key")
	if _, err := os.Stat(keyPath); err != nil { // 不在 run 里生成新私钥：那样必然与已签发的证书不匹配
		return fmt.Errorf("缺少 %s（先运行 spark-agent enroll）: %w", keyPath, err)
	}
	key, err := client.LoadOrCreateKey(keyPath)
	if err != nil {
		return err
	}
	certDER, err := readPEM(filepath.Join(*dir, "agent.crt"))
	if err != nil {
		return err
	}
	caDER, err := readPEM(filepath.Join(*dir, "ca.crt"))
	if err != nil {
		return err
	}
	tc, err := client.TLSFor(key, certDER, caDER, cfg.ServerName)
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(*stateDir); err != nil {
		return err
	}
	st, err := state.Open(filepath.Join(*stateDir, "state.json"))
	if err != nil {
		return err
	}
	var rt runtime.Runtime
	var dk disk.Disk
	var nf netfilter.Netfilter
	runner := cmdrun.Exec{}
	if *fake {
		rt, dk, nf = rtfake.New(), diskfake.New(4000<<30), netfake.New()
	} else {
		rt = &docker.Runtime{Run: runner, Bridge: "spark0", Subnet: "10.200.0.0/24"}
		dk = &loop.Disk{Run: runner, Stat: os.Stat, StateDir: *stateDir, Log: log}
		nf = &iptables.Netfilter{Run: runner, Log: log}
	}
	ex := executor.New(executor.Deps{Runtime: rt, Disk: dk, Net: nf, State: st, StateDir: *stateDir, Bridge: "spark0", LocalIPs: localIPs, Log: log}, false)
	if err := ex.Recover(ctx); err != nil {
		log.Error("recover", "err", err) // 继续运行，StateReport 会如实上报（如 missing_data）
	}
	collector := &metrics.Collector{Run: runner, ReadFile: os.ReadFile}
	hostname, _ := os.Hostname()
	c := client.New(client.Config{Addr: cfg.Platform, TLS: tc, Version: version, Hostname: hostname, LocalIPs: localIPs},
		ex, func(ctx context.Context) metrics.Metrics { return collector.Collect(ctx, ex.DataMount()) }, log)
	exDone := make(chan struct{})
	go func() { defer close(exDone); ex.Run(ctx) }()
	log.Info("spark-agent running", "platform", cfg.Platform, "version", version, "fake", *fake)
	err = c.Run(ctx)
	<-exDone // Client.Run 只在 ctx 结束（或 NoReconnect，生产不用）时返回
	return err
}

func readPEM(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s 不是 PEM", path)
	}
	return block.Bytes, nil
}

// localIPs 返回本机全部非回环 IPv4 地址。
func localIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}
