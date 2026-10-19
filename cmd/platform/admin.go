package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mdp/qrterminal/v3"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/secretbox"
)

const adminUsage = `用法：
  platform admin create --username <用户名> [--role super|ops|finance]
  platform admin reset-totp --username <用户名>
  platform admin gen-upstream-key --path <私钥文件>
  platform admin ssh-host-key
  platform admin app import <目录>`

// prompter 读取一行输入；secret 为 true 时不回显（用于密码）。
type prompter func(label string, secret bool) (string, error)

// runAdmin 处理 `platform admin ...` 子命令。
func runAdmin(ctx context.Context, svc *admin.Service, cat *appdeploy.Catalog, args []string, out io.Writer, prompt prompter) error {
	if len(args) == 0 {
		return errors.New(adminUsage)
	}
	switch args[0] {
	case "create":
		return adminCreate(ctx, svc, args[1:], out, prompt)
	case "reset-totp":
		return adminResetTOTP(ctx, svc, args[1:], out)
	case "gen-upstream-key":
		return adminGenUpstreamKey(args[1:], out)
	case "app":
		return appImport(ctx, cat, args[1:], out)
	}
	return errors.New(adminUsage)
}

func adminCreate(ctx context.Context, svc *admin.Service, args []string, out io.Writer, prompt prompter) error {
	fs := flag.NewFlagSet("admin create", flag.ContinueOnError)
	fs.SetOutput(out)
	username := fs.String("username", "", "管理员用户名")
	role := fs.String("role", string(admin.RoleSuper), "角色：super / ops / finance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("请用 --username 指定用户名\n" + adminUsage)
	}
	pw, err := prompt("密码（至少 12 个字符）：", true)
	if err != nil {
		return err
	}
	pw2, err := prompt("再次输入密码：", true)
	if err != nil {
		return err
	}
	if pw != pw2 {
		return errors.New("两次输入的密码不一致")
	}
	a, err := svc.Create(ctx, audit.Actor{}, *username, pw, admin.Role(*role))
	if err != nil {
		return err
	}
	uri, err := svc.EnrollTOTP(ctx, a.ID)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "\n请用验证器 App（如 Google Authenticator、Microsoft Authenticator）扫描下方二维码，或手动添加以下链接：\n%s\n\n", uri)
	qrterminal.GenerateHalfBlock(uri, qrterminal.L, out)
	for attempt := 0; attempt < 3; attempt++ {
		code, err := prompt("\n输入验证器显示的 6 位动态码：", false)
		if err != nil {
			return err
		}
		codes, err := svc.ConfirmEnrollment(ctx, a.ID, code)
		if err == nil {
			fmt.Fprintf(out, "\n管理员 %s（%s）已创建并绑定动态码。\n以下 %d 个恢复码每个只能用一次，请立即离线保存，之后不会再显示：\n",
				a.Username, a.Role, len(codes))
			for _, c := range codes {
				fmt.Fprintln(out, "  "+c)
			}
			return nil
		}
		if ae := apperr.As(err); ae == nil || ae.Code != "invalid_totp" {
			return err
		}
		fmt.Fprintln(out, "动态码不正确，请等验证器刷新出下一个动态码后重试。")
	}
	return errors.New("动态码连续 3 次错误：管理员已创建但尚未绑定动态码，首次登录后台时会要求重新绑定")
}

func adminResetTOTP(ctx context.Context, svc *admin.Service, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("admin reset-totp", flag.ContinueOnError)
	fs.SetOutput(out)
	username := fs.String("username", "", "管理员用户名")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		return errors.New("请用 --username 指定用户名\n" + adminUsage)
	}
	a, err := svc.FindByUsername(ctx, *username)
	if err != nil {
		return fmt.Errorf("找不到管理员 %s: %w", *username, err)
	}
	if err := svc.ResetTOTP(ctx, audit.Actor{}, a.ID); err != nil {
		return err
	}
	fmt.Fprintf(out, "已重置 %s 的动态码并注销其全部会话，该管理员下次登录时需要重新绑定。\n", a.Username)
	return nil
}

// terminalPrompter 从终端读取输入；密码不回显。输入不是终端（例如管道）时按行读取。
func terminalPrompter(in *os.File, out io.Writer) prompter {
	reader := bufio.NewReader(in)
	return func(label string, secret bool) (string, error) {
		fmt.Fprint(out, label)
		if secret && term.IsTerminal(int(in.Fd())) {
			b, err := term.ReadPassword(int(in.Fd()))
			fmt.Fprintln(out)
			return string(b), err
		}
		line, err := reader.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
}

func adminGenUpstreamKey(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("admin gen-upstream-key", flag.ContinueOnError)
	fs.SetOutput(out)
	path := fs.String("path", "", "私钥保存路径（将以 0600 写入，另生成 .pub）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("请用 --path 指定私钥保存路径\n" + adminUsage)
	}
	if err := pki.GenerateUpstreamKey(*path); err != nil {
		return err
	}
	pub, err := pki.LoadUpstreamPublicKey(*path)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "已生成上游 SSH 密钥：%s（请设置 SPARK_UPSTREAM_SSH_KEY_FILE=%s）\n公钥：%s\n", *path, *path, pub)
	return nil
}

// adminSSHHostKey 打印 SSH 网关主机公钥与指纹（写进帮助页「首次连接请核对指纹」）。
func adminSSHHostKey(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, out io.Writer) error {
	if _, err := pki.EnsureCA(ctx, pool, box, clock.Real{}); err != nil {
		return err
	}
	signer, err := pki.EnsureSSHHostKey(ctx, pool, box)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "公钥：%s\n指纹：%s\n", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), pki.SSHFingerprint(signer.PublicKey()))
	return nil
}
