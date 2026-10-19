package appdeploy_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

// ---- 辅助 ----

// specPorts 是 goodSpec 换成给定端口（第一个为主端口，带 /health）。
func specPorts(ports ...int) appdeploy.Spec {
	s := goodSpec()
	s.Ports = nil
	for i, p := range ports {
		ps := appdeploy.PortSpec{Port: p}
		if i == 0 {
			ps.HealthPath = "/health"
		}
		s.Ports = append(s.Ports, ps)
	}
	return s
}

// publishedApp 建一个已上架应用并发布 1.0.0 版本。
func (e *env) publishedApp(slug string, spec appdeploy.Spec, script string, imageIDs []int64) (appdeploy.App, appdeploy.Version) {
	e.t.Helper()
	a := e.app(slug, appdeploy.StatusPublished)
	return a, e.publish(a.ID, "1.0.0", spec, script, imageIDs)
}

func (e *env) publish(appID int64, ver string, spec appdeploy.Spec, script string, imageIDs []int64) appdeploy.Version {
	e.t.Helper()
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), appID, appdeploy.VersionInput{Version: ver, ImageIDs: imageIDs, Spec: spec, Script: script})
	if err != nil {
		e.t.Fatalf("CreateVersion %s: %v", ver, err)
	}
	v, _, err = e.cat.PublishVersion(e.ctx, e.actor(), appID, v.ID)
	if err != nil {
		e.t.Fatalf("PublishVersion %s: %v", ver, err)
	}
	return v
}

// instanceOf 让 uid 下单一台并把实例直接置为 state（假 Agent 结果的最小方式）。
func (e *env) instanceOf(uid int64, state string) rental.Instance {
	e.t.Helper()
	_, inst, err := e.rental.Place(e.ctx, rental.PlaceRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if err != nil {
		e.t.Fatalf("Place: %v", err)
	}
	e.exec(`UPDATE instances SET state = $2 WHERE id = $1`, inst.ID, state)
	e.exec(`UPDATE machines SET state = 'allocated' WHERE id = $1`, inst.MachineID)
	if state == rental.StateReserved {
		e.exec(`UPDATE instances SET scheduled_start_at = $2 WHERE id = $1`, inst.ID, e.clk.Now().Add(24*time.Hour))
	}
	inst.State = state
	return inst
}

func (e *env) runningInstance() rental.Instance  { return e.instanceOf(e.userID, rental.StateRunning) }
func (e *env) reservedInstance() rental.Instance { return e.instanceOf(e.userID, rental.StateReserved) }

func (e *env) otherImage() int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM images WHERE enabled AND id <> $1 ORDER BY sort, id LIMIT 1`, e.imageID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) create(inst rental.Instance, appID int64, vals map[string]string) appdeploy.Deployment {
	e.t.Helper()
	d, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: inst.UserID, InstanceID: inst.ID, AppID: appID, Env: vals, IP: "1.2.3.4"})
	if err != nil {
		e.t.Fatalf("Create app %d on %s: %v", appID, inst.ID, err)
	}
	return d
}

func (e *env) dep(id string) appdeploy.Deployment {
	e.t.Helper()
	d, err := e.svc.Get(e.ctx, id)
	if err != nil {
		e.t.Fatalf("Get %s: %v", id, err)
	}
	return d
}

func (e *env) addUserPort(instanceID string, port int) {
	e.exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, created_at, updated_at) VALUES ($1, $2, 'private', '', $3, $3)`,
		instanceID, port, e.clk.Now())
}

func (e *env) addAppPort(d appdeploy.Deployment, port int) {
	e.exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, deployment_id, created_at, updated_at) VALUES ($1, $2, 'private', 'app', $3, $4, $4)`,
		d.InstanceID, port, d.ID, e.clk.Now())
}

func (e *env) secrets(d appdeploy.Deployment) map[string]string {
	e.t.Helper()
	m, err := appdeploy.OpenSecrets(e.box, d.SecretEnvEnc)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

// ---- 用例 ----

func TestCreateQueuedDeployment(t *testing.T) {
	e := newEnv(t, 1)
	app, ver := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	e.clk.Advance(time.Minute)
	d := e.create(inst, app.ID, map[string]string{"API_KEY": "sk-secret-12345", "NOTE": "hello"})
	now := e.clk.Now()
	if d.State != appdeploy.StateQueued || d.Desired != appdeploy.DesiredRunning || d.Attempt != 0 || d.Port != 8000 ||
		!d.NextPollAt.Equal(now) || d.SpecHash != ver.SpecHash || d.AppVersionID != ver.ID || d.UserID != e.userID || d.InstanceID != inst.ID ||
		!strings.HasPrefix(d.ID, "dep-") || len(d.ID) != 16 || d.RowVersion != 0 || d.Health != appdeploy.HealthUnknown {
		t.Fatalf("deployment = %+v", d)
	}
	if d.Env["MODEL_ID"] != "Qwen/Qwen3-8B" || d.Env["MAX_MODEL_LEN"] != "32768" || d.Env["DEBUG"] != "false" || d.Env["NOTE"] != "hello" {
		t.Fatalf("env = %v", d.Env)
	}
	if _, leaked := d.Env["API_KEY"]; leaked || len(d.SecretEnvEnc) == 0 || strings.Contains(string(d.SecretEnvEnc), "sk-secret") {
		t.Fatalf("secret handling: env %v enc %q", d.Env, d.SecretEnvEnc)
	}
	if s := e.secrets(d); s["API_KEY"] != "sk-secret-12345" || len(s) != 1 {
		t.Fatalf("secrets = %v", s)
	}
	if got := e.dep(d.ID); got.ID != d.ID || got.RowVersion != 0 || got.Env["NOTE"] != "hello" {
		t.Fatalf("Get = %+v", got)
	}
	evs, err := e.svc.Events(e.ctx, d.ID, 10)
	if err != nil || len(evs) != 1 || evs[0].FromState != "" || evs[0].ToState != appdeploy.StateQueued || !evs[0].At.Equal(now) {
		t.Fatalf("events = %+v, %v", evs, err)
	}
	logs := e.auditLogs("instance.app_deploy", inst.ID)
	if len(logs) != 1 || logs[0].ActorType != "user" || logs[0].ActorID != e.userID || logs[0].IP != "1.2.3.4" ||
		strings.Contains(string(logs[0].Diff), "sk-secret") || strings.Contains(string(logs[0].Diff), "hello") || !strings.Contains(string(logs[0].Diff), d.ID) {
		t.Fatalf("audit = %+v", logs)
	}
	// 无 secret → secret_env_enc 为 NULL
	app2, _ := e.publishedApp("other", specPorts(9000), goodScript, []int64{e.imageID})
	if d2 := e.create(inst, app2.ID, nil); d2.SecretEnvEnc != nil {
		t.Fatalf("secret_env_enc without secrets = %q", d2.SecretEnvEnc)
	}
}

func TestCreateLimitsAndConflicts(t *testing.T) {
	e := newEnv(t, 4)
	var apps []appdeploy.App
	for i, slug := range []string{"a1", "a2", "a3", "a4"} {
		a, _ := e.publishedApp(slug, specPorts(8000+i), goodScript, []int64{e.imageID})
		apps = append(apps, a)
	}
	createErr := func(inst rental.Instance, appID int64) error {
		_, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: inst.UserID, InstanceID: inst.ID, AppID: appID})
		return err
	}
	inst := e.runningInstance()
	d1 := e.create(inst, apps[0].ID, nil)
	if err := createErr(inst, apps[0].ID); codeOf(err) != "app_exists" {
		t.Fatalf("same app again = %v", err)
	}
	e.create(inst, apps[1].ID, nil)
	e.create(inst, apps[2].ID, nil)
	if err := createErr(inst, apps[3].ID); codeOf(err) != "app_limit" {
		t.Fatalf("4th app = %v", err)
	}
	// removed 的旧部署不占名额：卸载后可再建
	if r, err := e.svc.Remove(e.ctx, e.userID, inst.ID, d1.ID, ""); err != nil || r.State != appdeploy.StateRemoved {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	if again := e.create(inst, apps[0].ID, nil); again.ID == d1.ID {
		t.Fatal("re-create returned the removed row")
	}
	if err := createErr(inst, apps[3].ID); codeOf(err) != "app_limit" {
		t.Fatalf("4th app after re-create = %v", err)
	}

	// 实例已有用户端口 8000
	inst2 := e.runningInstance()
	e.addUserPort(inst2.ID, 8000)
	if err := createErr(inst2, apps[0].ID); codeOf(err) != "app_port_conflict" || !strings.Contains(err.Error(), "8000") {
		t.Fatalf("user port conflict = %v", err)
	}

	// 另一部署（installing）声明的附加端口 = 新应用主端口
	inst3 := e.runningInstance()
	b, _ := e.publishedApp("bb", specPorts(8100, 8200), goodScript, []int64{e.imageID})
	c, _ := e.publishedApp("cc", specPorts(8200), goodScript, []int64{e.imageID})
	db := e.create(inst3, b.ID, nil)
	e.exec(`UPDATE app_deployments SET state = 'installing' WHERE id = $1`, db.ID)
	if err := createErr(inst3, c.ID); codeOf(err) != "app_port_conflict" || !strings.Contains(err.Error(), "8200") {
		t.Fatalf("declared port conflict = %v", err)
	}
	// 该部署失效（removed）后端口释放
	e.exec(`UPDATE app_deployments SET state = 'removed' WHERE id = $1`, db.ID)
	e.create(inst3, c.ID, nil)

	// 4 个用户端口 + 应用 2 个端口 > 5
	inst4 := e.runningInstance()
	for p := 5001; p <= 5004; p++ {
		e.addUserPort(inst4.ID, p)
	}
	two, _ := e.publishedApp("two", specPorts(8300, 8301), goodScript, []int64{e.imageID})
	if err := createErr(inst4, two.ID); codeOf(err) != "port_limit" {
		t.Fatalf("port limit = %v", err)
	}
	one, _ := e.publishedApp("one", specPorts(8400), goodScript, []int64{e.imageID})
	e.create(inst4, one.ID, nil) // 4 + 1 = 5 正好
	if n := e.count(`SELECT count(*) FROM app_deployments WHERE instance_id = $1`, inst4.ID); n != 1 {
		t.Fatalf("deployments on inst4 = %d", n)
	}
}

func TestCreateImageMismatch(t *testing.T) {
	e := newEnv(t, 1)
	img2 := e.otherImage()
	inst := e.runningInstance()
	a, _ := e.publishedApp("needs-other", goodSpec(), goodScript, []int64{img2})
	_, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: inst.ID, AppID: a.ID})
	want1, _ := e.catalog.ImageName(e.ctx, img2)
	want2, _ := e.catalog.ImageName(e.ctx, e.imageID)
	if codeOf(err) != "app_image_mismatch" || !strings.Contains(err.Error(), want1) || !strings.Contains(err.Error(), want2) {
		t.Fatalf("mismatch = %v (want %q / %q)", err, want1, want2)
	}
	b, _ := e.publishedApp("multi", goodSpec(), goodScript, []int64{img2, e.imageID})
	if d := e.create(inst, b.ID, nil); d.State != appdeploy.StateQueued {
		t.Fatalf("multi-image = %+v", d)
	}
}

func TestCreateOnReservedAndNotOwner(t *testing.T) {
	e := newEnv(t, 4)
	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	createErr := func(inst rental.Instance, appID int64, vid *int64) error {
		_, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: inst.ID, AppID: appID, VersionID: vid})
		return err
	}
	res := e.reservedInstance()
	if d := e.create(res, a.ID, nil); d.State != appdeploy.StateQueued {
		t.Fatalf("reserved = %+v", d)
	}
	rel := e.runningInstance()
	e.exec(`UPDATE instances SET state = 'released' WHERE id = $1`, rel.ID)
	if err := createErr(rel, a.ID, nil); codeOf(err) != "invalid_state" {
		t.Fatalf("released = %v", err)
	}
	other := dbtest.CreateUser(t, e.pool, "13800000009", true)
	dbtest.Fund(t, e.pool, other, 1_000_000)
	theirs := e.instanceOf(other, rental.StateRunning)
	if err := createErr(theirs, a.ID, nil); codeOf(err) != "not_found" {
		t.Fatalf("not owner = %v", err)
	}
	inst := e.runningInstance()
	draft := e.app("draft", "")
	e.version(draft.ID, "1.0.0")
	if err := createErr(inst, draft.ID, nil); codeOf(err) != "app_not_found" {
		t.Fatalf("draft app = %v", err)
	}
	retired, _ := e.publishedApp("retired", goodSpec(), goodScript, []int64{e.imageID})
	e.exec(`UPDATE apps SET status = 'retired' WHERE id = $1`, retired.ID)
	if err := createErr(inst, retired.ID, nil); codeOf(err) != "app_not_found" {
		t.Fatalf("retired app = %v", err)
	}
	gone, _ := e.publishedApp("gone", goodSpec(), goodScript, []int64{e.imageID})
	e.exec(`UPDATE app_versions SET status = 'retired' WHERE app_id = $1`, gone.ID)
	if err := createErr(inst, gone.ID, nil); codeOf(err) != "app_unavailable" {
		t.Fatalf("all versions retired = %v", err)
	}
	b, bv := e.publishedApp("bbb", specPorts(9000), goodScript, []int64{e.imageID})
	if err := createErr(inst, a.ID, &bv.ID); codeOf(err) != "app_not_found" {
		t.Fatalf("foreign version = %v", err)
	}
	if err := createErr(inst, 999999, nil); codeOf(err) != "app_not_found" {
		t.Fatalf("missing app = %v", err)
	}
	// 指定本应用的已发布版本可以
	if d, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: inst.ID, AppID: b.ID, VersionID: &bv.ID}); err != nil || d.AppVersionID != bv.ID {
		t.Fatalf("explicit version = %+v, %v", d, err)
	}
}

func TestEnvValidationFailsCreate(t *testing.T) {
	e := newEnv(t, 1)
	s := goodSpec()
	s.Env[3].Required = true // NOTE
	a, _ := e.publishedApp("vllm", s, goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	_, err := e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: inst.ID, AppID: a.ID})
	if codeOf(err) != "app_env_required" || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("required missing = %v", err)
	}
	_, err = e.svc.Create(e.ctx, appdeploy.CreateRequest{UserID: e.userID, InstanceID: inst.ID, AppID: a.ID, Env: map[string]string{"NOTE": "x", "BOGUS": "1"}})
	if codeOf(err) != "app_env_invalid" {
		t.Fatalf("extra key = %v", err)
	}
	if n := e.count(`SELECT count(*) FROM app_deployments`); n != 0 {
		t.Fatalf("rows after failed creates = %d", n)
	}
}

func TestStateMatrix(t *testing.T) {
	e := newEnv(t, 1)
	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	states := []string{appdeploy.StateQueued, appdeploy.StateInstalling, appdeploy.StateRunning, appdeploy.StateStopped,
		appdeploy.StateFailed, appdeploy.StateRemoving, appdeploy.StateRemoved, appdeploy.StateLost}
	type op struct {
		name    string
		allowed map[string]string // state → 操作后的 state
		call    func(id string) (appdeploy.Deployment, error)
	}
	ops := []op{
		{"Redeploy", map[string]string{"queued": "queued", "running": "queued", "stopped": "queued", "failed": "queued"},
			func(id string) (appdeploy.Deployment, error) {
				return e.svc.Redeploy(e.ctx, appdeploy.RedeployRequest{UserID: e.userID, InstanceID: inst.ID, DeploymentID: id})
			}},
		{"Stop", map[string]string{"installing": "installing", "running": "running"},
			func(id string) (appdeploy.Deployment, error) { return e.svc.Stop(e.ctx, e.userID, inst.ID, id, "") }},
		{"Start", map[string]string{"stopped": "queued"},
			func(id string) (appdeploy.Deployment, error) { return e.svc.Start(e.ctx, e.userID, inst.ID, id, "") }},
		{"Remove", map[string]string{"queued": "removing", "installing": "removing", "running": "removing", "stopped": "removing", "failed": "removing", "removing": "removing"},
			func(id string) (appdeploy.Deployment, error) { return e.svc.Remove(e.ctx, e.userID, inst.ID, id, "") }},
	}
	for _, o := range ops {
		for _, st := range states {
			e.exec(`UPDATE app_deployments SET state = 'removed' WHERE instance_id = $1 AND state NOT IN ('removed', 'lost')`, inst.ID)
			d := e.create(inst, a.ID, nil)
			// attempt = 1：queued 不按「从未执行」处理，走常规卸载路径
			e.exec(`UPDATE app_deployments SET state = $2, attempt = 1, error_code = 'timeout' WHERE id = $1`, d.ID, st)
			before := e.dep(d.ID)
			got, err := o.call(d.ID)
			want, ok := o.allowed[st]
			if !ok {
				if codeOf(err) != "app_invalid_state" {
					t.Fatalf("%s on %s = %+v, %v; want app_invalid_state", o.name, st, got, err)
				}
				if after := e.dep(d.ID); after.RowVersion != before.RowVersion || after.State != st {
					t.Fatalf("%s on %s changed the row: %+v", o.name, st, after)
				}
				continue
			}
			if err != nil || got.ID != d.ID || got.State != want {
				t.Fatalf("%s on %s = %+v, %v; want state %s", o.name, st, got, err, want)
			}
			// 返回的就是库里的新行
			if fresh := e.dep(d.ID); fresh.RowVersion != got.RowVersion || fresh.State != got.State || fresh.Desired != got.Desired {
				t.Fatalf("%s on %s returned %+v, db %+v", o.name, st, got, fresh)
			}
			switch {
			case o.name == "Stop" && got.Desired != appdeploy.DesiredStopped,
				o.name == "Start" && (got.Desired != appdeploy.DesiredRunning || got.ErrorCode != "" || !got.NextPollAt.Equal(e.clk.Now())),
				o.name == "Remove" && st != "removing" && got.Desired != appdeploy.DesiredRemoved,
				o.name == "Redeploy" && st != "queued" && (got.ErrorCode != "" || got.Attempt != 1):
				t.Fatalf("%s on %s fields = %+v", o.name, st, got)
			}
			noop := (o.name == "Redeploy" && st == "queued") || (o.name == "Remove" && st == "removing")
			if noop != (got.RowVersion == before.RowVersion) {
				t.Fatalf("%s on %s row_version %d → %d (noop %v)", o.name, st, before.RowVersion, got.RowVersion, noop)
			}
		}
	}
	// 每个成功的写操作都有审计
	for action, n := range map[string]int{"instance.app_redeploy": 3, "instance.app_stop": 2, "instance.app_start": 1, "instance.app_remove": 5} {
		if got := len(e.auditLogs(action, inst.ID)); got != n {
			t.Fatalf("audit %s = %d, want %d", action, got, n)
		}
	}
	// 别人的部署 / 不在该实例上 → not_found
	if _, err := e.svc.Stop(e.ctx, e.userID+1000, inst.ID, "dep-nope", ""); codeOf(err) != "not_found" {
		t.Fatalf("stranger = %v", err)
	}
	if _, err := e.svc.Stop(e.ctx, e.userID, inst.ID, "dep-nope", ""); codeOf(err) != "not_found" {
		t.Fatalf("missing deployment = %v", err)
	}
}

func TestRedeployVersionAndSecrets(t *testing.T) {
	e := newEnv(t, 1)
	a, v1 := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	d := e.create(inst, a.ID, map[string]string{"API_KEY": "sk-secret-12345", "NOTE": "n1"})
	fail := func() {
		e.exec(`UPDATE app_deployments SET state = 'failed', error_code = 'timeout', attempt = 2 WHERE id = $1`, d.ID)
	}
	redeploy := func(req appdeploy.RedeployRequest) (appdeploy.Deployment, error) {
		req.UserID, req.InstanceID, req.DeploymentID = e.userID, inst.ID, d.ID
		return e.svc.Redeploy(e.ctx, req)
	}
	fail()
	e.clk.Advance(time.Minute)
	got, err := redeploy(appdeploy.RedeployRequest{})
	if err != nil || got.State != appdeploy.StateQueued || got.ErrorCode != "" || got.Attempt != 2 || got.Env["NOTE"] != "n1" ||
		e.secrets(got)["API_KEY"] != "sk-secret-12345" || !got.NextPollAt.Equal(e.clk.Now()) || got.FinishedAt != nil {
		t.Fatalf("redeploy keep-all = %+v, %v", got, err)
	}
	evs, _ := e.svc.Events(e.ctx, d.ID, 10)
	if len(evs) != 2 || evs[0].FromState != appdeploy.StateFailed || evs[0].ToState != appdeploy.StateQueued || !strings.Contains(evs[0].Detail, v1.Version) {
		t.Fatalf("events = %+v", evs)
	}
	fail()
	got, err = redeploy(appdeploy.RedeployRequest{EnvSet: true, Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}})
	if err != nil || got.Env["MODEL_ID"] != "Qwen/Qwen3-4B" || got.Env["NOTE"] != "" || e.secrets(got)["API_KEY"] != "sk-secret-12345" {
		t.Fatalf("redeploy env set, secret omitted = %+v, %v", got, err)
	}
	if _, has := got.Env["NOTE"]; has {
		t.Fatalf("NOTE should be dropped when EnvSet without it: %v", got.Env)
	}
	fail()
	got, err = redeploy(appdeploy.RedeployRequest{EnvSet: true, Env: map[string]string{"API_KEY": ""}})
	if err != nil || got.SecretEnvEnc != nil {
		t.Fatalf("redeploy clear secret = %+v, %v", got, err)
	}

	// v2 换端口：旧端口行删除，版本 / hash / 端口跟随
	e.addAppPort(got, 8000)
	v2 := e.publish(a.ID, "2.0.0", specPorts(9000), goodScript, []int64{e.imageID})
	fail()
	got, err = redeploy(appdeploy.RedeployRequest{})
	if err != nil || got.AppVersionID != v2.ID || got.SpecHash != v2.SpecHash || got.Port != 9000 {
		t.Fatalf("redeploy to v2 = %+v, %v", got, err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1`, d.ID); n != 0 {
		t.Fatalf("old port rows = %d", n)
	}

	// v3 镜像不兼容 → 报错且行不变
	e.publish(a.ID, "3.0.0", goodSpec(), goodScript, []int64{e.otherImage()})
	fail()
	before := e.dep(d.ID)
	if _, err := redeploy(appdeploy.RedeployRequest{}); codeOf(err) != "app_image_mismatch" {
		t.Fatalf("redeploy to v3 = %v", err)
	}
	if after := e.dep(d.ID); after.RowVersion != before.RowVersion || after.AppVersionID != v2.ID || after.State != appdeploy.StateFailed {
		t.Fatalf("row changed after mismatch: %+v", after)
	}
	// 指定 v1：回到 8000
	got, err = redeploy(appdeploy.RedeployRequest{VersionID: &v1.ID})
	if err != nil || got.AppVersionID != v1.ID || got.Port != 8000 {
		t.Fatalf("redeploy to v1 = %+v, %v", got, err)
	}
	// 已是 queued：再点一次返回当前行，不报错
	again, err := redeploy(appdeploy.RedeployRequest{})
	if err != nil || again.RowVersion != got.RowVersion || again.State != appdeploy.StateQueued {
		t.Fatalf("redeploy queued = %+v, %v", again, err)
	}
	// queued 时带变量：只更新变量
	again, err = redeploy(appdeploy.RedeployRequest{EnvSet: true, Env: map[string]string{"NOTE": "n2"}})
	if err != nil || again.Env["NOTE"] != "n2" || again.AppVersionID != v1.ID || again.RowVersion != got.RowVersion+1 {
		t.Fatalf("redeploy queued with env = %+v, %v", again, err)
	}
	// 变量非法 → 报错
	fail()
	if _, err := redeploy(appdeploy.RedeployRequest{VersionID: &v1.ID, EnvSet: true, Env: map[string]string{"MODEL_ID": "nope"}}); codeOf(err) != "app_env_invalid" {
		t.Fatalf("bad env = %v", err)
	}
	if logs := e.auditLogs("instance.app_redeploy", inst.ID); len(logs) != 6 || strings.Contains(string(logs[0].Diff), "sk-secret") {
		t.Fatalf("redeploy audit = %d", len(logs))
	}
}

func TestRemoveWhenInstanceStopped(t *testing.T) {
	e := newEnv(t, 3)
	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	running := func(inst rental.Instance) appdeploy.Deployment {
		d := e.create(inst, a.ID, nil)
		e.exec(`UPDATE app_deployments SET state = 'running', attempt = 1 WHERE id = $1`, d.ID)
		d.State, d.Attempt = appdeploy.StateRunning, 1
		e.addAppPort(d, 8000)
		return d
	}
	stopped := e.runningInstance()
	d := running(stopped)
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, stopped.ID)
	e.clk.Advance(time.Minute)
	got, err := e.svc.Remove(e.ctx, e.userID, stopped.ID, d.ID, "5.6.7.8")
	if err != nil || got.State != appdeploy.StateRemoved || got.Desired != appdeploy.DesiredRemoved || !got.CleanupPending ||
		got.FinishedAt == nil || !got.FinishedAt.Equal(e.clk.Now()) {
		t.Fatalf("remove on stopped instance = %+v, %v", got, err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, stopped.ID); n != 0 {
		t.Fatalf("port rows left = %d", n)
	}
	logs := e.auditLogs("instance.port_delete", stopped.ID)
	if len(logs) != 1 || logs[0].ActorType != "user" || logs[0].ActorID != e.userID || !strings.Contains(string(logs[0].Diff), "卸载应用") {
		t.Fatalf("port_delete audit = %+v", logs)
	}
	if evs, _ := e.svc.Events(e.ctx, d.ID, 10); len(evs) != 2 || evs[0].ToState != appdeploy.StateRemoved || !strings.Contains(evs[0].Detail, "下次启动") {
		t.Fatalf("events = %+v", evs)
	}

	run := e.runningInstance()
	d2 := running(run)
	got, err = e.svc.Remove(e.ctx, e.userID, run.ID, d2.ID, "")
	if err != nil || got.State != appdeploy.StateRemoving || got.Desired != appdeploy.DesiredRemoved || got.CleanupPending || got.FinishedAt != nil {
		t.Fatalf("remove on running instance = %+v, %v", got, err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1`, d2.ID); n != 1 {
		t.Fatalf("port rows on removing = %d", n)
	}
	// 暂停的实例按停机处理
	e.exec(`UPDATE app_deployments SET state = 'running', desired = 'running' WHERE id = $1`, d2.ID)
	e.exec(`UPDATE instances SET suspended_at = $2 WHERE id = $1`, run.ID, e.clk.Now())
	if got, err = e.svc.Remove(e.ctx, e.userID, run.ID, d2.ID, ""); err != nil || got.State != appdeploy.StateRemoved || !got.CleanupPending {
		t.Fatalf("remove on suspended instance = %+v, %v", got, err)
	}

	res := e.reservedInstance()
	d3 := e.create(res, a.ID, nil)
	got, err = e.svc.Remove(e.ctx, e.userID, res.ID, d3.ID, "")
	if err != nil || got.State != appdeploy.StateRemoved || got.CleanupPending {
		t.Fatalf("remove fresh queued on reserved = %+v, %v", got, err)
	}
}

func TestAdminForceRemoveAndLists(t *testing.T) {
	e := newEnv(t, 2)
	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	b, _ := e.publishedApp("bbb", specPorts(9000), goodScript, []int64{e.imageID})
	i1, i2 := e.runningInstance(), e.runningInstance()
	d1 := e.create(i1, a.ID, map[string]string{"API_KEY": "sk-secret-12345"})
	e.clk.Advance(time.Second)
	d2 := e.create(i1, b.ID, nil)
	d3 := e.create(i2, a.ID, nil)
	e.exec(`UPDATE app_deployments SET state = 'running', attempt = 1 WHERE id = $1`, d1.ID)
	e.addAppPort(d1, 8000)

	if _, err := e.svc.AdminForceRemove(e.ctx, audit.Actor{AdminID: 7}, d1.ID, " "); codeOf(err) != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	got, err := e.svc.AdminForceRemove(e.ctx, audit.Actor{AdminID: 7}, d1.ID, "违规")
	if err != nil || got.State != appdeploy.StateRemoving || got.Desired != appdeploy.DesiredRemoved {
		t.Fatalf("force remove = %+v, %v", got, err)
	}
	logs := e.auditLogs("app.deployment.force_remove", d1.ID)
	if len(logs) != 1 || logs[0].ActorType != "admin" || strings.Contains(string(logs[0].Diff), "sk-secret") || !strings.Contains(string(logs[0].Diff), "违规") {
		t.Fatalf("force audit = %+v", logs)
	}
	if evs, _ := e.svc.Events(e.ctx, d1.ID, 10); evs[0].Detail != "平台卸载：违规" {
		t.Fatalf("event = %+v", evs[0])
	}
	e.exec(`UPDATE app_deployments SET state = 'removed' WHERE id = $1`, d1.ID)
	if _, err := e.svc.AdminForceRemove(e.ctx, audit.Actor{AdminID: 7}, d1.ID, "x"); codeOf(err) != "app_invalid_state" {
		t.Fatalf("force remove terminal = %v", err)
	}

	all, err := e.svc.ListForInstance(e.ctx, i1.ID)
	if err != nil || len(all) != 2 || all[0].ID != d1.ID || all[1].ID != d2.ID {
		t.Fatalf("ListForInstance = %+v, %v", all, err)
	}
	live, err := e.svc.ListLive(e.ctx, i1.ID)
	if err != nil || len(live) != 1 || live[0].ID != d2.ID {
		t.Fatalf("ListLive = %+v, %v", live, err)
	}
	by, err := e.svc.LiveByInstances(e.ctx, []string{i1.ID, i2.ID, "ins-none"})
	if err != nil || len(by) != 2 || len(by[i1.ID]) != 1 || by[i2.ID][0].ID != d3.ID {
		t.Fatalf("LiveByInstances = %+v, %v", by, err)
	}
	if _, err := e.svc.GetForUser(e.ctx, e.userID, i2.ID, d2.ID); codeOf(err) != "not_found" {
		t.Fatalf("GetForUser wrong instance = %v", err)
	}
	if _, err := e.svc.GetForUser(e.ctx, e.userID+1, i1.ID, d2.ID); codeOf(err) != "not_found" {
		t.Fatalf("GetForUser stranger = %v", err)
	}
	if _, err := e.svc.Get(e.ctx, "dep-missing"); codeOf(err) != "not_found" {
		t.Fatalf("Get missing = %v", err)
	}
	list, total, err := e.svc.AdminList(e.ctx, appdeploy.ListFilter{AppID: a.ID}, paging.Page{Number: 1, Size: 1})
	if err != nil || total != 2 || len(list) != 1 || list[0].ID != d3.ID {
		t.Fatalf("AdminList app = %+v, %d, %v", list, total, err)
	}
	list, total, err = e.svc.AdminList(e.ctx, appdeploy.ListFilter{State: "queued", InstanceID: i1.ID, UserID: e.userID}, paging.Page{Number: 1, Size: 20})
	if err != nil || total != 1 || list[0].ID != d2.ID {
		t.Fatalf("AdminList state = %+v, %d, %v", list, total, err)
	}
	if _, _, err := e.svc.AdminList(e.ctx, appdeploy.ListFilter{State: "bogus"}, paging.Page{Number: 1, Size: 20}); codeOf(err) != "bad_request" {
		t.Fatalf("AdminList bad state = %v", err)
	}
}

func TestPortURLFor(t *testing.T) {
	e := newEnv(t, 1)
	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	d := e.create(inst, a.ID, nil)
	if u, err := e.svc.PortURLFor(e.ctx, d); err != nil || u != "" {
		t.Fatalf("no port row = %q, %v", u, err)
	}
	e.addAppPort(d, 8000)
	if u, err := e.svc.PortURLFor(e.ctx, d); err != nil || u != "https://"+inst.ID+"-8000.apps.test/" {
		t.Fatalf("with port row = %q, %v", u, err)
	}
}

func TestLogLiveAndFallback(t *testing.T) {
	e := newEnv(t, 1)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: signer.PublicKey(), Shell: true, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	up := upstream.New(upstream.Config{Signer: signer, Port: srv.Port(), DialTimeout: 500 * time.Millisecond}, e.rental)
	t.Cleanup(up.Close)
	appRoot := filepath.Join(srv.Root(), "root", "data")
	svc := e.newService(up, appRoot)

	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	e.exec(`UPDATE machines SET lan_ip = '127.0.0.1' WHERE id = $1`, inst.MachineID)
	e.exec(`UPDATE instances SET ssh_host_pubkey = $2 WHERE id = $1`, inst.ID, srv.HostPubLine())
	d := e.create(inst, a.ID, map[string]string{"API_KEY": "sk-secret-12345"})
	e.clk.Advance(time.Minute)
	e.exec(`UPDATE app_deployments SET log_tail = 'from db', last_polled_at = $2 WHERE id = $1`, d.ID, e.clk.Now())
	dir := appdeploy.AppDir(appRoot, "vllm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "log"), []byte("starting with key sk-secret-12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	res, err := svc.Log(e.ctx, e.userID, inst.ID, d.ID)
	if err != nil || !res.Live || res.Log != "starting with key ***\n" || !res.At.Equal(e.clk.Now()) {
		t.Fatalf("live log = %+v, %v", res, err)
	}
	// 实例停机 → 不 exec，返回库里的
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, inst.ID)
	if res, err = svc.Log(e.ctx, e.userID, inst.ID, d.ID); err != nil || res.Live || res.Log != "from db" {
		t.Fatalf("stopped instance log = %+v, %v", res, err)
	}
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, inst.ID)
	// 容器 sshd 不可达 → 回落到 log_tail
	srv.Close()
	res, err = svc.Log(e.ctx, e.userID, inst.ID, d.ID)
	if err != nil || res.Live || res.Log != "from db" || !res.At.Equal(e.clk.Now().Add(-time.Minute)) {
		t.Fatalf("fallback log = %+v, %v", res, err)
	}
	if _, err := svc.Log(e.ctx, e.userID+1, inst.ID, d.ID); codeOf(err) != "not_found" {
		t.Fatalf("stranger log = %v", err)
	}
}

func TestRedact(t *testing.T) {
	got := appdeploy.Redact("key=abcdefgh and abcdefghij", map[string]string{"A": "abcdefgh", "B": "abcdefghij"})
	if got != "key=*** and ***" {
		t.Fatalf("Redact = %q", got)
	}
	if got := appdeploy.Redact("short abc1234 stays", map[string]string{"A": "abc1234"}); got != "short abc1234 stays" {
		t.Fatalf("short secret = %q", got)
	}
	if got := appdeploy.Redact("nothing", nil); got != "nothing" {
		t.Fatalf("nil secrets = %q", got)
	}
}

func TestSealSecretsRoundTrip(t *testing.T) {
	e := newEnv(t, 0)
	if appdeploy.SealSecrets(e.box, nil) != nil || appdeploy.SealSecrets(e.box, map[string]string{}) != nil {
		t.Fatal("empty map should seal to nil")
	}
	enc := appdeploy.SealSecrets(e.box, map[string]string{"K": "v-value-1"})
	m, err := appdeploy.OpenSecrets(e.box, enc)
	if err != nil || m["K"] != "v-value-1" {
		t.Fatalf("open = %v, %v", m, err)
	}
	if m, err := appdeploy.OpenSecrets(e.box, nil); err != nil || len(m) != 0 {
		t.Fatalf("open nil = %v, %v", m, err)
	}
}

func TestMarkFailedAndRemoved(t *testing.T) {
	e := newEnv(t, 1)
	a, _ := e.publishedApp("vllm", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	d := e.create(inst, a.ID, nil)
	e.exec(`UPDATE app_deployments SET state = 'installing' WHERE id = $1`, d.ID)
	d = e.dep(d.ID)
	e.clk.Advance(time.Minute)
	now := e.clk.Now()
	if err := e.svc.MarkFailed(e.ctx, e.pool, d, appdeploy.ErrCodeTimeout, strings.Repeat("长", 300), now); err != nil {
		t.Fatal(err)
	}
	got := e.dep(d.ID)
	if got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeTimeout || len([]rune(got.Message)) != appdeploy.MaxMessageLen ||
		got.FinishedAt == nil || !got.FinishedAt.Equal(now) || !got.NextPollAt.Equal(now.Add(24*time.Hour)) || got.RowVersion != d.RowVersion+1 {
		t.Fatalf("markFailed = %+v", got)
	}
	evs, _ := e.svc.Events(e.ctx, d.ID, 1)
	if evs[0].FromState != appdeploy.StateInstalling || evs[0].ToState != appdeploy.StateFailed || !strings.HasPrefix(evs[0].Detail, "部署超时：长") {
		t.Fatalf("failed event = %+v", evs[0])
	}
	e.addAppPort(got, 8000)
	if err := e.svc.MarkRemoved(e.ctx, e.pool, got, appdeploy.ErrCodeInstanceGone, "实例已释放", now); err != nil {
		t.Fatal(err)
	}
	rm := e.dep(d.ID)
	if rm.State != appdeploy.StateRemoved || rm.Desired != appdeploy.DesiredRemoved || rm.ErrorCode != appdeploy.ErrCodeInstanceGone || rm.CleanupPending ||
		rm.RowVersion != got.RowVersion+1 {
		t.Fatalf("markRemoved = %+v", rm)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1`, d.ID); n != 0 {
		t.Fatalf("ports left = %d", n)
	}
	if logs := e.auditLogs("instance.port_delete", inst.ID); len(logs) != 1 || logs[0].ActorID != e.userID {
		t.Fatalf("port_delete audit = %+v", logs)
	}
}

func TestRegisterPorts(t *testing.T) {
	e := newEnv(t, 1)
	a, _ := e.publishedApp("vllm", specPorts(8000, 8001), goodScript, []int64{e.imageID})
	b, _ := e.publishedApp("bb", specPorts(9000), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	d := e.create(inst, a.ID, nil)
	other := e.create(inst, b.ID, nil)
	now := e.clk.Now()
	ports := specPorts(8000, 8001).Ports
	// 8000：换机复制来的无主行 → 认领；8001：不存在 → 插入
	e.addUserPort(inst.ID, 8000)
	code, err := appdeploy.RegisterPorts(e.ctx, e.pool, d, ports, "vLLM", now)
	if err != nil || code != "" {
		t.Fatalf("register = %q, %v", code, err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1 AND note = 'vLLM' AND visibility = 'private'`, d.ID); n != 2 {
		t.Fatalf("registered rows = %d", n)
	}
	// 再次进入 running：自己的行不变，不报冲突
	e.exec(`UPDATE instance_ports SET note = 'mine' WHERE instance_id = $1 AND port = 8001`, inst.ID)
	if code, err := appdeploy.RegisterPorts(e.ctx, e.pool, d, ports, "vLLM", now); err != nil || code != "" {
		t.Fatalf("re-register = %q, %v", code, err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND port = 8001 AND note = 'mine'`, inst.ID); n != 1 {
		t.Fatal("own row note overwritten")
	}
	// 属于别的部署 → port_conflict，不写任何行
	if code, err := appdeploy.RegisterPorts(e.ctx, e.pool, other, specPorts(9000, 8000).Ports, "bb", now); err != nil || code != appdeploy.ErrCodePortConflict {
		t.Fatalf("conflict = %q, %v", code, err)
	}
	if n := e.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1`, other.ID); n != 0 {
		t.Fatalf("rows written on conflict = %d", n)
	}
	// 总数超限 → port_limit
	for p := 5001; p <= 5003; p++ {
		e.addUserPort(inst.ID, p)
	}
	if code, err := appdeploy.RegisterPorts(e.ctx, e.pool, other, specPorts(9000).Ports, "bb", now); err != nil || code != appdeploy.ErrCodePortLimit {
		t.Fatalf("limit = %q, %v", code, err)
	}
}
