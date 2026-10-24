package comfywf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/storage"
)

// 本包的数据库测试共用一个嵌入式 PG（每个测试一个全新库）；纯逻辑测试不受影响。
func TestMain(m *testing.M) { dbtest.Main(m) }

// catEnv 是目录服务测试的公共环境：嵌入式 PG、假时钟、本地磁盘文件存储。
type catEnv struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	clk     *clock.Fake
	uploads *storage.Service
	cat     *Catalog
}

func newCatEnv(t *testing.T) *catEnv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	disk, err := storage.NewLocalDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	up := storage.NewService(pool, clk, disk)
	return &catEnv{t: t, ctx: context.Background(), pool: pool, clk: clk, uploads: up,
		cat: NewCatalog(pool, clk, up, slog.New(slog.NewTextHandler(io.Discard, nil)))}
}

func (e *catEnv) actor() audit.Actor { return audit.Actor{AdminID: 1} }

func (e *catEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *catEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// workflow 建一个 txt2img 工作流（status 为空 = draft）。
func (e *catEnv) workflow(slug, status string) Workflow {
	e.t.Helper()
	w, err := e.cat.CreateWorkflow(e.ctx, e.actor(), WorkflowInput{Slug: slug, Name: slug, Category: CategoryTxt2Img, Status: status})
	if err != nil {
		e.t.Fatalf("CreateWorkflow %s: %v", slug, err)
	}
	return w
}

// goodVersion 与 testdata/graph_sdxl_ok.json + api_sdxl.json 配套的合法版本输入。
func goodVersion(t *testing.T, ver string) VersionInput {
	return VersionInput{Version: ver, Graph: fixture(t, "graph_sdxl_ok.json"), APIPrompt: fixture(t, "api_sdxl.json"), Manifest: okManifest(),
		Changelog: "说明 " + ver}
}

// version 在工作流下建一个合法 draft 版本。
func (e *catEnv) version(wfID int64, ver string) Version {
	e.t.Helper()
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), wfID, goodVersion(e.t, ver))
	if err != nil {
		e.t.Fatalf("CreateVersion %s: %v", ver, err)
	}
	return v
}

// published 建并发布一个版本。
func (e *catEnv) published(wfID int64, ver string) Version {
	e.t.Helper()
	v, err := e.cat.PublishVersion(e.ctx, e.actor(), wfID, e.version(wfID, ver).ID)
	if err != nil {
		e.t.Fatalf("PublishVersion %s: %v", ver, err)
	}
	return v
}

// deployFixture 建一套能挂 deployment_workflows 的行（用户、实例、comfyui 应用与版本），返回 newDep 建部署（指定 state）。
func (e *catEnv) deployFixture() func(state string) string {
	e.t.Helper()
	uid := dbtest.CreateUser(e.t, e.pool, "13800000001", true)
	pid, iid := dbtest.ProductID(e.t, e.pool), dbtest.ImageID(e.t, e.pool)
	e.exec(`INSERT INTO apps (slug, name, category, status, created_at, updated_at) VALUES ('comfyui', 'ComfyUI', 'image', 'published', $1, $1)`, dbtest.T0)
	e.exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, created_at, updated_at)
		VALUES ((SELECT id FROM apps WHERE slug = 'comfyui'), '1.1.0', 'published', '{1}', '{"ports":[{"port":8188}]}', 'true', 'h', $1, $1)`, dbtest.T0)
	n := 0
	return func(state string) string {
		n++
		// 每台机器一个实例、每个实例一个 comfyui 部署（instances_machine_active_uniq / app_deployments_live_uniq）
		id, ins := fmt.Sprintf("dep-%012d", n), fmt.Sprintf("ins-wf%06d", n)
		mid := dbtest.CreateMachine(e.t, e.pool, fmt.Sprintf("SN-WF%03d", n))
		e.exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'running', 'x', $6, $6)`, ins, uid, mid, pid, iid, dbtest.T0)
		e.exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
			VALUES ($1, $2, $3, (SELECT id FROM apps WHERE slug = 'comfyui'), (SELECT id FROM app_versions LIMIT 1), $4, 8188, 'h', $5, $5, $5)`,
			id, ins, uid, state, dbtest.T0)
		return id
	}
}

// attach 在部署上挂一个工作流版本（指定安装状态）。
func (e *catEnv) attach(dep string, v Version, state string) {
	e.t.Helper()
	e.exec(`INSERT INTO deployment_workflows (deployment_id, workflow_id, workflow_version_id, state, added_at) VALUES ($1, $2, $3, $4, $5)`,
		dep, v.WorkflowID, v.ID, state, dbtest.T0)
}

func errCode(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := errCode(err); got != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
}
