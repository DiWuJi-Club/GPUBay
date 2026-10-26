package appdeploy_test

import (
	"bytes"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// ---- 辅助 ----

// clusterSpec 是 goodSpec 加上 cluster 段。
func clusterSpec(mode string, minSize, maxSize int) appdeploy.Spec {
	s := goodSpec()
	s.Cluster = &appdeploy.ClusterSupport{Mode: mode, MinSize: minSize, MaxSize: maxSize}
	return s
}

// clusterOf 把给定实例组成一个 active 集群（rank 按参数顺序；system 时成员与集群都归系统账号），返回集群 ID。
// 不经 rental 的集群分配（T6）：直接写 instance_clusters 与 instances.cluster_id / cluster_rank。
func (e *env) clusterOf(system bool, insts ...rental.Instance) string {
	e.t.Helper()
	e.exec(`INSERT INTO cluster_fabrics (name, kind, isolation, created_at, updated_at) VALUES ('测试以太网', 'lan', 'routed', $1, $1) ON CONFLICT (name) DO NOTHING`, e.clk.Now())
	k := e.count(`SELECT count(*) FROM instance_clusters`)
	id := fmt.Sprintf("clu-t%09d", k)
	uid := e.userID
	if system {
		uid = e.systemID
	}
	e.exec(`INSERT INTO instance_clusters (id, user_id, system, size, fabric_id, mode, rdma, subnet_index, state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, (SELECT id FROM cluster_fabrics WHERE name = '测试以太网'), 'routed', false, $5, 'active', $6, $6)`,
		id, uid, system, len(insts), k, e.clk.Now())
	for i, inst := range insts {
		e.exec(`UPDATE instances SET cluster_id = $2, cluster_rank = $3, user_id = $4, system = $5 WHERE id = $1`, inst.ID, id, i, uid, system)
	}
	return id
}

// newCluster 建 n 台 running 实例并组成用户集群。
func (e *env) newCluster(n int) (string, []rental.Instance) {
	e.t.Helper()
	insts := make([]rental.Instance, n)
	for i := range insts {
		insts[i] = e.runningInstance()
	}
	return e.clusterOf(false, insts...), insts
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if ae := apperr.As(err); ae == nil || ae.Code != code {
		t.Fatalf("%s = %v, want %s", what, err, code)
	}
}

func (e *env) createCluster(clusterID string, appID int64, vals map[string]string) appdeploy.ClusterDeployment {
	e.t.Helper()
	cd, err := e.svc.CreateCluster(e.ctx, appdeploy.ClusterCreateRequest{UserID: e.userID, ClusterID: clusterID, AppID: appID, Env: vals, IP: "1.2.3.4"})
	if err != nil {
		e.t.Fatalf("CreateCluster %s app %d: %v", clusterID, appID, err)
	}
	return cd
}

func (e *env) clusterDep(id string) appdeploy.ClusterDeployment {
	e.t.Helper()
	cd, err := e.svc.GetClusterDeployment(e.ctx, id)
	if err != nil {
		e.t.Fatalf("GetClusterDeployment %s: %v", id, err)
	}
	return cd
}

// ---- 创建 ----

func TestCreateClusterValidation(t *testing.T) {
	e := newEnv(t, 5)
	opt, _ := e.publishedApp("vllm-node", clusterSpec(appdeploy.ClusterModeOptional, 2, 4), goodScript, []int64{e.imageID})
	none, _ := e.publishedApp("webui", specPorts(8080), goodScript, []int64{e.imageID})
	big, _ := e.publishedApp("big", clusterSpec(appdeploy.ClusterModeRequired, 4, 4), goodScript, []int64{e.imageID})
	mismatch, _ := e.publishedApp("other", clusterSpec(appdeploy.ClusterModeOptional, 2, 4), goodScript, []int64{e.otherImage()})
	dup, _ := e.publishedApp("dup", func() appdeploy.Spec { s := clusterSpec(appdeploy.ClusterModeOptional, 2, 4); s.Ports[0].Port = 9000; return s }(), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(2)
	other := dbtest.CreateUser(t, e.pool, "13800000001", true)

	try := func(uid int64, clusterID string, appID int64) error {
		_, err := e.svc.CreateCluster(e.ctx, appdeploy.ClusterCreateRequest{UserID: uid, ClusterID: clusterID, AppID: appID, IP: "1.2.3.4"})
		return err
	}
	wantCode(t, "missing cluster", try(e.userID, "clu-nonexist00", opt.ID), "cluster_not_found")
	wantCode(t, "not owner", try(other, cid, opt.ID), "cluster_not_found")

	e.exec(`UPDATE instance_clusters SET state = 'released' WHERE id = $1`, cid)
	wantCode(t, "cluster released", try(e.userID, cid, opt.ID), "invalid_state")
	e.exec(`UPDATE instance_clusters SET state = 'active' WHERE id = $1`, cid)

	e.exec(`UPDATE instances SET state = 'wiping' WHERE id = $1`, insts[1].ID)
	wantCode(t, "member missing", try(e.userID, cid, opt.ID), "invalid_state")
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, insts[1].ID)

	wantCode(t, "no cluster support", try(e.userID, cid, none.ID), "app_cluster_unsupported")
	err := try(e.userID, cid, big.ID)
	wantCode(t, "size out of range", err, "app_cluster_size")
	if ae := apperr.As(err); ae.Message != "该应用支持 4～4 台的集群" || ae.Status != 400 {
		t.Fatalf("app_cluster_size = %+v", ae)
	}
	wantCode(t, "image mismatch", try(e.userID, cid, mismatch.ID), "app_image_mismatch")
	single := e.create(insts[1], dup.ID, nil) // optional 应用可以单实例部署；之后集群部署同一应用 → 成员上已有
	wantCode(t, "member has app", try(e.userID, cid, dup.ID), "app_exists")
	if _, err := e.svc.Remove(e.ctx, e.userID, insts[1].ID, single.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateCluster(e.ctx, appdeploy.ClusterCreateRequest{UserID: e.userID, ClusterID: cid, AppID: opt.ID,
		Env: map[string]string{"MODEL_ID": "bogus"}}); apperr.As(err) == nil || apperr.As(err).Code != "app_env_invalid" {
		t.Fatalf("bad env = %v", err)
	}
	if n := e.count(`SELECT count(*) FROM app_cluster_deployments`); n != 0 {
		t.Fatalf("rejected creates left %d cluster deployments", n)
	}

	e.createCluster(cid, opt.ID, nil)
	wantCode(t, "second cluster app", try(e.userID, cid, dup.ID), "app_cluster_exists")

	// 系统集群：只有系统账号能部署（内部应用亦然）；普通用户按不存在处理
	sysInst := e.runningInstance()
	sysInst2 := e.runningInstance()
	sys := e.clusterOf(true, sysInst, sysInst2)
	wantCode(t, "user on system cluster", try(e.userID, sys, opt.ID), "cluster_not_found")
	internal, _ := e.publishedApp("vllm-int", func() appdeploy.Spec { s := clusterSpec(appdeploy.ClusterModeOptional, 2, 4); s.Ports[0].Port = 8100; return s }(), goodScript, []int64{e.imageID})
	e.exec(`UPDATE apps SET internal = true WHERE id = $1`, internal.ID)
	if _, err := e.svc.CreateCluster(e.ctx, appdeploy.ClusterCreateRequest{UserID: e.systemID, ClusterID: sys, AppID: internal.ID}); err != nil {
		t.Fatalf("system CreateCluster internal app: %v", err)
	}
	wantCode(t, "user internal app", try(e.userID, cid, internal.ID), "app_not_found") // 内部应用对普通用户按不存在处理
}

func TestCreateClusterRoles(t *testing.T) {
	e := newEnv(t, 4)
	a, v := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(4)
	vals := map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "API_KEY": "sk-secret-12345"}
	cd := e.createCluster(cid, a.ID, vals)
	if cd.ClusterID != cid || cd.UserID != e.userID || cd.AppID != a.ID || cd.AppVersionID != v.ID || cd.State != appdeploy.ClusterDepActive ||
		cd.Status != appdeploy.ClusterStatusInstalling || cd.Health != appdeploy.HealthUnknown || len(cd.Members) != 4 {
		t.Fatalf("cluster deployment = %+v", cd)
	}
	for i, m := range cd.Members {
		d := m.Deployment
		wantRole := appdeploy.RoleWorker
		if i == 0 {
			wantRole = appdeploy.RoleHead
		}
		if m.Rank != i || m.InstanceID != insts[i].ID || m.Role != wantRole || !m.Current || d.ClusterRole != wantRole ||
			d.ClusterDeploymentID == nil || *d.ClusterDeploymentID != cd.ID || d.State != appdeploy.StateQueued || d.UserID != e.userID ||
			d.AppVersionID != v.ID || d.SpecHash != v.SpecHash {
			t.Fatalf("member %d = %+v", i, m)
		}
		if !maps.Equal(d.Env, cd.Members[0].Deployment.Env) || e.secrets(d)["API_KEY"] != "sk-secret-12345" || d.Env["MODEL_ID"] != "Qwen/Qwen3-4B" {
			t.Fatalf("member %d env = %v", i, d.Env)
		}
		if n := e.count(`SELECT count(*) FROM app_deploy_events WHERE deployment_id = $1`, d.ID); n != 1 {
			t.Fatalf("member %d events = %d", i, n)
		}
	}
	logs := e.auditLogs("cluster.app_deploy", cid)
	if len(logs) != 1 {
		t.Fatalf("audit = %+v", logs)
	}
	if n := e.count(`SELECT count(*) FROM audit_logs WHERE action = 'instance.app_deploy'`); n != 0 {
		t.Fatalf("per-instance deploy audits = %d", n)
	}
	if bytes.Contains(logs[0].Diff, []byte("sk-secret")) || logs[0].TargetType != "cluster" || logs[0].ActorType != "user" {
		t.Fatal("audit leaks secret")
	}
	live, err := e.svc.LiveClusterDeployment(e.ctx, cid)
	if err != nil || live == nil || live.ID != cd.ID || len(live.Members) != 4 {
		t.Fatalf("LiveClusterDeployment = %+v, %v", live, err)
	}
	if got, err := e.svc.ClusterDeploymentForUser(e.ctx, e.userID, cid, cd.ID); err != nil || got.ID != cd.ID {
		t.Fatalf("ClusterDeploymentForUser = %+v, %v", got, err)
	}
	other := dbtest.CreateUser(t, e.pool, "13800000001", true)
	if _, err := e.svc.ClusterDeploymentForUser(e.ctx, other, cid, cd.ID); err != apperr.ErrNotFound {
		t.Fatalf("other user = %v", err)
	}
	if _, err := e.svc.ClusterDeploymentForUser(e.ctx, e.userID, "clu-other00000", cd.ID); err != apperr.ErrNotFound {
		t.Fatalf("wrong cluster = %v", err)
	}
	if _, err := e.svc.GetClusterDeployment(e.ctx, "acd-nonexistent0"); err != apperr.ErrNotFound {
		t.Fatalf("missing = %v", err)
	}
	if none, err := e.svc.LiveClusterDeployment(e.ctx, "clu-other00000"); none != nil || err != nil {
		t.Fatalf("no live = %+v, %v", none, err)
	}
	// 成员部署的视图带角色与集群部署 ID；目录视图带 cluster 段
	views, err := e.svc.Views(e.ctx, []appdeploy.Deployment{cd.Members[0].Deployment, cd.Members[1].Deployment})
	if err != nil || views[0].ClusterRole != "head" || views[1].ClusterRole != "worker" || views[0].ClusterDeploymentID == nil || *views[0].ClusterDeploymentID != cd.ID {
		t.Fatalf("views = %+v, %v", views, err)
	}
	cat, err := e.svc.CatalogViews(e.ctx, false)
	if err != nil || len(cat) != 1 || cat[0].Version.Cluster == nil || cat[0].Version.Cluster.Mode != "required" || cat[0].Version.Cluster.MaxSize != 4 {
		t.Fatalf("catalog = %+v, %v", cat, err)
	}
}

func TestCreateClusterConcurrent(t *testing.T) {
	e := newEnv(t, 2)
	a, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	b, _ := e.publishedApp("vllm-node", func() appdeploy.Spec { s := clusterSpec(appdeploy.ClusterModeOptional, 2, 4); s.Ports[0].Port = 9000; return s }(), goodScript, []int64{e.imageID})
	cid, _ := e.newCluster(2)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, appID := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.CreateCluster(e.ctx, appdeploy.ClusterCreateRequest{UserID: e.userID, ClusterID: cid, AppID: appID})
		}()
	}
	wg.Wait()
	ok, exists := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case apperr.As(err) != nil && apperr.As(err).Code == "app_cluster_exists":
			exists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || exists != 1 || e.count(`SELECT count(*) FROM app_cluster_deployments`) != 1 || e.count(`SELECT count(*) FROM app_deployments`) != 2 {
		t.Fatalf("ok %d exists %d errs %v", ok, exists, errs)
	}
}

// ---- required 应用与成员部署的单实例接口 ----

func TestRequiredAppRejectsSingleCreate(t *testing.T) {
	e := newEnv(t, 2)
	req, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	opt, _ := e.publishedApp("vllm-node", func() appdeploy.Spec { s := clusterSpec(appdeploy.ClusterModeOptional, 2, 4); s.Ports[0].Port = 9000; return s }(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	_, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: inst.ID, AppID: req.ID})
	wantCode(t, "single create required", err, "app_cluster_required")
	if ae := apperr.As(err); ae.Status != 400 {
		t.Fatalf("status = %d", ae.Status)
	}
	if d := e.create(inst, opt.ID, nil); d.ClusterDeploymentID != nil || d.ClusterRole != "" {
		t.Fatalf("optional single deployment = %+v", d)
	}
}

func TestRequiredAppRejectsPrepareInstall(t *testing.T) {
	e := newEnv(t, 2)
	e.rental.SetAppHooks(e.svc)
	req, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	_, err := e.svc.PrepareInstall(e.ctx, e.pool, req.ID, e.imageID, nil, nil)
	wantCode(t, "PrepareInstall required", err, "app_cluster_required")
	// 下单预装（一次多台）同样被拒，整单不下
	insts := e.count(`SELECT count(*) FROM instances`)
	appID := req.ID
	_, err = e.rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: 2, Duration: catalog.D1d, AppID: &appID})
	wantCode(t, "PlaceBatch required preinstall", err, "app_cluster_required")
	if e.count(`SELECT count(*) FROM instances`) != insts || e.count(`SELECT count(*) FROM app_deployments`) != 0 {
		t.Fatal("rejected order left side effects")
	}
	opt, _ := e.publishedApp("vllm-node", func() appdeploy.Spec { s := clusterSpec(appdeploy.ClusterModeOptional, 2, 4); s.Ports[0].Port = 9000; return s }(), goodScript, []int64{e.imageID})
	if _, err := e.svc.PrepareInstall(e.ctx, e.pool, opt.ID, e.imageID, nil, nil); err != nil {
		t.Fatalf("optional PrepareInstall: %v", err)
	}
}

func TestMemberDeploymentUserOpsManaged(t *testing.T) {
	e := newEnv(t, 2)
	a, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, nil)
	for i, m := range cd.Members {
		d := m.Deployment
		e.setDep(d.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
		inst := insts[i].ID
		_, err := e.svc.Redeploy(e.ctx, appdeploy.RedeployRequest{UserID: e.userID, InstanceID: inst, DeploymentID: d.ID})
		wantCode(t, "Redeploy member", err, "app_cluster_managed")
		_, err = e.svc.Stop(e.ctx, e.userID, inst, d.ID, "")
		wantCode(t, "Stop member", err, "app_cluster_managed")
		e.setDep(d.ID, appdeploy.StateStopped, appdeploy.DesiredStopped)
		_, err = e.svc.Start(e.ctx, e.userID, inst, d.ID, "")
		wantCode(t, "Start member", err, "app_cluster_managed")
		_, err = e.svc.Remove(e.ctx, e.userID, inst, d.ID, "")
		wantCode(t, "Remove member", err, "app_cluster_managed")
		if ae := apperr.As(err); ae.Status != 409 || ae.Message != "该部署属于集群应用，请在集群页操作" {
			t.Fatalf("managed = %+v", ae)
		}
		// 只读接口照常
		if got, err := e.svc.GetForUser(e.ctx, e.userID, inst, d.ID); err != nil || got.ID != d.ID {
			t.Fatalf("GetForUser = %v", err)
		}
		if _, err := e.svc.Log(e.ctx, e.userID, inst, d.ID); err != nil {
			t.Fatalf("Log = %v", err)
		}
		if evs, err := e.svc.Events(e.ctx, d.ID, 0); err != nil || len(evs) == 0 {
			t.Fatalf("Events = %v %v", evs, err)
		}
		if got := e.dep(d.ID); got.State != appdeploy.StateStopped {
			t.Fatalf("member changed by rejected op: %+v", got)
		}
	}
}

// ---- 整组操作 ----

func TestStopStartRemoveCluster(t *testing.T) {
	e := newEnv(t, 3)
	a, v := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, nil)
	other := dbtest.CreateUser(t, e.pool, "13800000001", true)
	if _, err := e.svc.StopCluster(e.ctx, other, cd.ID, ""); err != apperr.ErrNotFound {
		t.Fatalf("other user stop = %v", err)
	}
	// 全员 queued：没有可停的
	_, err := e.svc.StopCluster(e.ctx, e.userID, cd.ID, "")
	wantCode(t, "stop queued", err, "app_invalid_state")
	for _, m := range cd.Members {
		e.setDep(m.Deployment.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	}
	if got := e.clusterDep(cd.ID); got.Status != appdeploy.ClusterStatusRunning || got.Health != appdeploy.HealthOK {
		t.Fatalf("all running = %+v", got)
	}
	got, err := e.svc.StopCluster(e.ctx, e.userID, cd.ID, "5.6.7.8")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got.Members {
		if m.Deployment.Desired != appdeploy.DesiredStopped || m.Deployment.State != appdeploy.StateRunning {
			t.Fatalf("after stop = %+v", m.Deployment)
		}
	}
	// Runner 执行 stop 后
	for _, m := range cd.Members {
		e.setDep(m.Deployment.ID, appdeploy.StateStopped, appdeploy.DesiredStopped)
	}
	if got := e.clusterDep(cd.ID); got.Status != appdeploy.ClusterStatusStopped {
		t.Fatalf("all stopped = %+v", got.Status)
	}
	got, err = e.svc.StartCluster(e.ctx, e.userID, cd.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got.Members {
		if m.Deployment.State != appdeploy.StateQueued || m.Deployment.Desired != appdeploy.DesiredRunning {
			t.Fatalf("after start = %+v", m.Deployment)
		}
	}
	_, err = e.svc.StartCluster(e.ctx, e.userID, cd.ID, "")
	wantCode(t, "start again", err, "app_invalid_state")

	// 卸载：rank0 实例 running → removing（Runner 执行 remove）；rank1 实例停机 → 直接 removed
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, insts[1].ID)
	got, err = e.svc.RemoveCluster(e.ctx, e.userID, cd.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != appdeploy.ClusterDepRemoved || got.Status != appdeploy.ClusterStatusRemoving ||
		got.Members[0].Deployment.State != appdeploy.StateRemoving || got.Members[1].Deployment.State != appdeploy.StateRemoved {
		t.Fatalf("after remove = %+v", got)
	}
	again, err := e.svc.RemoveCluster(e.ctx, e.userID, cd.ID, "")
	if err != nil || again.State != appdeploy.ClusterDepRemoved {
		t.Fatalf("remove again = %+v, %v", again, err)
	}
	if live, err := e.svc.LiveClusterDeployment(e.ctx, cid); live != nil || err != nil {
		t.Fatalf("live after remove = %+v, %v", live, err)
	}
	if err := e.svc.MarkRemoved(e.ctx, e.pool, e.dep(got.Members[0].Deployment.ID), "", "已卸载", e.clk.Now()); err != nil {
		t.Fatal(err)
	}
	if got := e.clusterDep(cd.ID); got.Status != appdeploy.ClusterStatusRemoved {
		t.Fatalf("all removed = %+v", got.Status)
	}
	for _, action := range []string{"cluster.app_deploy", "cluster.app_stop", "cluster.app_start", "cluster.app_remove"} {
		if logs := e.auditLogs(action, cid); len(logs) != 1 {
			t.Fatalf("%s audits = %d", action, len(logs))
		}
	}
	// 卸载后可以重新部署（新的集群部署）
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, insts[1].ID)
	if cd2 := e.createCluster(cid, a.ID, nil); cd2.ID == cd.ID || cd2.AppVersionID != v.ID || len(cd2.Members) != 2 {
		t.Fatalf("redeploy after remove = %+v", cd2)
	}
	_, err = e.svc.StopCluster(e.ctx, e.userID, cd.ID, "")
	wantCode(t, "stop removed", err, "app_invalid_state")
}

// 成员换机：复制到新实例的 queued 部署保留角色 → 聚合 stale → RedeployCluster 后旧实例上的部署 removed、全员 queued。
func TestRedeployClusterAfterMigration(t *testing.T) {
	e := newEnv(t, 4)
	a, v1 := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "API_KEY": "sk-secret-12345"})
	for _, m := range cd.Members {
		e.setDep(m.Deployment.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
		e.exec(`UPDATE app_deployments SET health = 'ok' WHERE id = $1`, m.Deployment.ID)
	}
	// 换机（T10 的 AdminMigrate 对成员的效果）：新实例接替 rank1，旧实例洗机
	oldW := insts[1]
	newW := e.runningInstance()
	e.exec(`UPDATE instances SET cluster_id = $2, cluster_rank = 1 WHERE id = $1`, newW.ID, cid)
	e.hookTx(oldW.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceMigrated(e.ctx, tx, oldW.ID, newW.ID, e.clk.Now()) })
	e.exec(`UPDATE instances SET state = 'wiping' WHERE id = $1`, oldW.ID)
	e.hookTx(oldW.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceGone(e.ctx, tx, oldW.ID, "洗机", e.clk.Now()) })

	got := e.clusterDep(cd.ID)
	if got.Status != appdeploy.ClusterStatusStale || got.State != appdeploy.ClusterDepActive || len(got.Members) != 3 {
		t.Fatalf("after migrate = %+v", got)
	}
	var copied appdeploy.ClusterMemberDeployment
	for _, m := range got.Members {
		if m.InstanceID == newW.ID {
			copied = m
		}
		if m.InstanceID == oldW.ID && (m.Current || m.Deployment.State != appdeploy.StateLost) {
			t.Fatalf("old member = %+v", m)
		}
	}
	if !copied.Current || copied.Rank != 1 || copied.Role != appdeploy.RoleWorker || copied.Deployment.State != appdeploy.StateQueued ||
		copied.Deployment.ClusterDeploymentID == nil || *copied.Deployment.ClusterDeploymentID != cd.ID {
		t.Fatalf("copied member = %+v", copied)
	}
	// 复制来的部署跑起来了也仍是 stale：head 的分布式任务已失效
	e.setDep(copied.Deployment.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	if s := e.clusterDep(cd.ID).Status; s != appdeploy.ClusterStatusStale {
		t.Fatalf("copied running status = %s", s)
	}

	v2 := e.publish(a.ID, "1.1.0", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript+"# v2\n", []int64{e.imageID})
	re, err := e.svc.RedeployCluster(e.ctx, appdeploy.ClusterRedeployRequest{UserID: e.userID, ClusterDeploymentID: cd.ID, IP: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if re.Status != appdeploy.ClusterStatusInstalling || re.AppVersionID != v2.ID || len(re.Members) != 2 {
		t.Fatalf("after redeploy = %+v", re)
	}
	for i, m := range re.Members {
		d := m.Deployment
		if !m.Current || m.Rank != i || d.State != appdeploy.StateQueued || d.AppVersionID != v2.ID || d.SpecHash != v2.SpecHash ||
			d.Env["MODEL_ID"] != "Qwen/Qwen3-4B" || e.secrets(d)["API_KEY"] != "sk-secret-12345" {
			t.Fatalf("member %d after redeploy = %+v", i, m)
		}
	}
	if re.Members[1].InstanceID != newW.ID {
		t.Fatalf("rank1 = %s", re.Members[1].InstanceID)
	}
	if n := e.count(`SELECT count(*) FROM app_deployments WHERE instance_id = $1 AND state = 'removed' AND cluster_deployment_id = $2`, oldW.ID, cd.ID); n != 1 {
		t.Fatalf("old member deployment not removed: %d", n)
	}
	if logs := e.auditLogs("cluster.app_redeploy", cid); len(logs) != 1 {
		t.Fatalf("redeploy audits = %d", len(logs))
	}
	_ = v1

	// 有成员在 installing → 整组重新部署被拒
	e.setDep(re.Members[0].Deployment.ID, appdeploy.StateInstalling, appdeploy.DesiredRunning)
	_, err = e.svc.RedeployCluster(e.ctx, appdeploy.ClusterRedeployRequest{UserID: e.userID, ClusterDeploymentID: cd.ID})
	wantCode(t, "redeploy while installing", err, "app_invalid_state")
}

// 换机时未复制（failed）的成员：RedeployCluster 为当前成员补建部署。
func TestRedeployClusterBackfillsMissingMember(t *testing.T) {
	e := newEnv(t, 3)
	a, v := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, nil)
	e.setDep(cd.Members[0].Deployment.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	e.setDep(cd.Members[1].Deployment.ID, appdeploy.StateFailed, appdeploy.DesiredRunning)
	if s := e.clusterDep(cd.ID).Status; s != appdeploy.ClusterStatusFailed {
		t.Fatalf("one failed = %s", s)
	}
	oldW, newW := insts[1], e.runningInstance()
	e.exec(`UPDATE instances SET cluster_id = $2, cluster_rank = 1 WHERE id = $1`, newW.ID, cid)
	e.hookTx(oldW.ID, func(tx pgx.Tx) error { return e.svc.OnInstanceMigrated(e.ctx, tx, oldW.ID, newW.ID, e.clk.Now()) })
	e.exec(`UPDATE instances SET state = 'wiping' WHERE id = $1`, oldW.ID)
	if got := e.clusterDep(cd.ID); got.Status != appdeploy.ClusterStatusStale {
		t.Fatalf("after migrate without copy = %+v", got)
	}
	re, err := e.svc.RedeployCluster(e.ctx, appdeploy.ClusterRedeployRequest{UserID: e.userID, ClusterDeploymentID: cd.ID, VersionID: &v.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(re.Members) != 2 || re.Members[1].InstanceID != newW.ID || re.Members[1].Deployment.State != appdeploy.StateQueued ||
		re.Members[1].Role != appdeploy.RoleWorker || re.Members[0].Deployment.State != appdeploy.StateQueued || re.Status != appdeploy.ClusterStatusInstalling {
		t.Fatalf("backfilled = %+v", re)
	}
}

// 成员全部洗机（开通期中止 / 集群结束）：最后一个成员部署没了 → 集群部署 removed。
func TestClusterDeploymentFinalizedWhenMembersGone(t *testing.T) {
	e := newEnv(t, 2)
	a, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, insts := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, nil)
	e.hookTx(insts[0].ID, func(tx pgx.Tx) error { return e.svc.OnInstanceGone(e.ctx, tx, insts[0].ID, "开通失败", e.clk.Now()) })
	e.exec(`UPDATE instances SET state = 'wiping' WHERE id = $1`, insts[0].ID)
	if got := e.clusterDep(cd.ID); got.State != appdeploy.ClusterDepActive || got.Status != appdeploy.ClusterStatusStale {
		t.Fatalf("one member gone = %+v", got)
	}
	e.hookTx(insts[1].ID, func(tx pgx.Tx) error { return e.svc.OnInstanceGone(e.ctx, tx, insts[1].ID, "开通失败", e.clk.Now()) })
	if got := e.clusterDep(cd.ID); got.State != appdeploy.ClusterDepRemoved || got.Status != appdeploy.ClusterStatusRemoved {
		t.Fatalf("all members gone = %+v", got)
	}
}
