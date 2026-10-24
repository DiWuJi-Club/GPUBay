package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// 真实 Service 满足 llm 的小接口（app.go 装配时用）。
var (
	_ llm.RentalOps = (*rental.Service)(nil)
	_ llm.AppOps    = (*appdeploy.Service)(nil)
)

// fakeRental 是落库的假 rental：PlaceSystem 真插 instances（system）+ active lease、机器 allocated（满足外键与候选查询），
// 状态由测试经 setInstance 推进；记录调用。
type fakeRental struct {
	e  *env
	mu sync.Mutex

	placed   []rental.SystemPlaceRequest
	released []string
	placeErr error // 非 nil：PlaceSystem 直接返回它
}

func (f *fakeRental) PlaceSystem(ctx context.Context, req rental.SystemPlaceRequest) (rental.Instance, error) {
	f.mu.Lock()
	f.placed = append(f.placed, req)
	perr := f.placeErr
	f.mu.Unlock()
	if perr != nil {
		return rental.Instance{}, perr
	}
	now := f.e.clk.Now()
	id := randx.ID("ins-", 8)
	err := db.WithTx(ctx, f.e.pool, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT state = 'idle' AND agent_online AND group_id = $2
			AND NOT EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))
			AND NOT EXISTS (SELECT 1 FROM machine_leases WHERE machine_id = $1 AND status IN ('reserved', 'active') AND upper(period) > $3)
			FROM machines WHERE id = $1 FOR NO KEY UPDATE`, req.MachineID, req.GroupID, now).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return apperr.ErrMachineUnavailable
		}
		if _, err := tx.Exec(ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, password_auth,
			ssh_host_pubkey, group_id, system, created_at, updated_at)
			SELECT $1, $2, m.id, m.product_id, $3, 'creating', '\x00', false, 'ssh-ed25519 AAAA', $4, true, $5, $5 FROM machines m WHERE m.id = $6`,
			id, f.e.systemUserID, req.ImageID, req.GroupID, now, req.MachineID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
			VALUES ($1, $2, tstzrange($3, $4), 'active', $3, $3)`, req.MachineID, id, now, now.Add(rental.SystemLeaseHorizon)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE machines SET state = 'allocated' WHERE id = $1`, req.MachineID)
		return err
	})
	if err != nil {
		return rental.Instance{}, err
	}
	return f.Get(ctx, id)
}

func (f *fakeRental) ReleaseSystem(ctx context.Context, id string) error {
	f.mu.Lock()
	f.released = append(f.released, id)
	f.mu.Unlock()
	tag, err := f.e.pool.Exec(ctx, `UPDATE instances SET state = 'wiping' WHERE id = $1 AND system AND state IN ('creating', 'running', 'stopped')`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.ErrInvalidState
	}
	return nil
}

func (f *fakeRental) Get(ctx context.Context, id string) (rental.Instance, error) {
	var i rental.Instance
	err := f.e.pool.QueryRow(ctx, `SELECT id, user_id, machine_id, product_id, image_id, state, suspended_at, created_at, system
		FROM instances WHERE id = $1`, id).Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.SuspendedAt,
		&i.CreatedAt, &i.System)
	if db.IsNoRows(err) {
		return rental.Instance{}, apperr.ErrNotFound
	}
	return i, err
}

func (f *fakeRental) UpstreamTarget(ctx context.Context, id string) (upstream.Target, error) {
	if _, err := f.Get(ctx, id); err != nil {
		return upstream.Target{}, err
	}
	return upstream.Target{InstanceID: id, LanIP: "10.0.0.10"}, nil
}

func (f *fakeRental) ListLiveSystemInstances(ctx context.Context) ([]rental.Instance, error) {
	rows, err := f.e.pool.Query(ctx, `SELECT id FROM instances WHERE system AND state IN ('creating', 'running', 'stopped', 'wiping') ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]rental.Instance, 0, len(ids))
	for _, id := range ids {
		i, err := f.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, nil
}

func (f *fakeRental) placeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.placed)
}

func (f *fakeRental) releasedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// fakeApps 是落库的假 appdeploy：Create 插 queued 部署，Redeploy 按真实规则（running / stopped / failed → queued；queued 无操作），
// 状态由测试经 setDeployment 推进；记录调用。
type fakeApps struct {
	e  *env
	mu sync.Mutex

	creates     []appdeploy.CreateRequest
	redeploys   []appdeploy.RedeployRequest
	redeployErr error // 非 nil：Redeploy 直接返回它
}

func (f *fakeApps) Create(ctx context.Context, req appdeploy.CreateRequest) (appdeploy.Deployment, error) {
	f.mu.Lock()
	f.creates = append(f.creates, req)
	f.mu.Unlock()
	ver := f.e.versionID
	if req.VersionID != nil {
		ver = *req.VersionID
	}
	var exists bool
	if err := f.e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_deployments WHERE instance_id = $1 AND app_id = $2
		AND state NOT IN ('removed', 'lost'))`, req.InstanceID, req.AppID).Scan(&exists); err != nil {
		return appdeploy.Deployment{}, err
	}
	if exists {
		return appdeploy.Deployment{}, apperr.ErrAppExists
	}
	env, _ := json.Marshal(req.Env)
	id := randx.ID("dep-", 12)
	now := f.e.clk.Now()
	if _, err := f.e.pool.Exec(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, env, port, spec_hash,
		next_poll_at, created_at, updated_at) VALUES ($1, $2, $3, $4, $5, 'queued', $6, 8000, 'x', $7, $7, $7)`,
		id, req.InstanceID, req.UserID, req.AppID, ver, env, now); err != nil {
		return appdeploy.Deployment{}, err
	}
	return f.Get(ctx, id)
}

func (f *fakeApps) Redeploy(ctx context.Context, req appdeploy.RedeployRequest) (appdeploy.Deployment, error) {
	f.mu.Lock()
	f.redeploys = append(f.redeploys, req)
	rerr := f.redeployErr
	f.mu.Unlock()
	if rerr != nil {
		return appdeploy.Deployment{}, rerr
	}
	d, err := f.Get(ctx, req.DeploymentID)
	if err != nil {
		return d, err
	}
	switch d.State {
	case appdeploy.StateQueued:
		return d, nil
	case appdeploy.StateRunning, appdeploy.StateStopped, appdeploy.StateFailed:
	default:
		return appdeploy.Deployment{}, apperr.ErrAppInvalidState
	}
	f.e.setDeployment(req.DeploymentID, appdeploy.StateQueued, appdeploy.HealthUnknown, "")
	return f.Get(ctx, req.DeploymentID)
}

func (f *fakeApps) Get(ctx context.Context, id string) (appdeploy.Deployment, error) {
	var d appdeploy.Deployment
	var env []byte
	err := f.e.pool.QueryRow(ctx, `SELECT id, instance_id, user_id, app_id, app_version_id, state, phase, progress, message, health, error_code, env
		FROM app_deployments WHERE id = $1`, id).Scan(&d.ID, &d.InstanceID, &d.UserID, &d.AppID, &d.AppVersionID, &d.State, &d.Phase,
		&d.Progress, &d.Message, &d.Health, &d.ErrorCode, &env)
	if db.IsNoRows(err) {
		return d, apperr.ErrNotFound
	}
	if err == nil {
		err = json.Unmarshal(env, &d.Env)
	}
	return d, err
}

func (f *fakeApps) ListLive(ctx context.Context, instanceID string) ([]appdeploy.Deployment, error) {
	rows, err := f.e.pool.Query(ctx, `SELECT id FROM app_deployments WHERE instance_id = $1 AND state NOT IN ('removed', 'lost') ORDER BY created_at, id`, instanceID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []appdeploy.Deployment
	for _, id := range ids {
		d, err := f.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeApps) Events(ctx context.Context, id string, limit int) ([]appdeploy.Event, error) {
	return []appdeploy.Event{{ID: 1, DeploymentID: id, At: f.e.clk.Now(), ToState: "queued", Detail: "创建部署"}}, nil
}

func (f *fakeApps) createCalls() []appdeploy.CreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appdeploy.CreateRequest(nil), f.creates...)
}

func (f *fakeApps) redeployCalls() []appdeploy.RedeployRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appdeploy.RedeployRequest(nil), f.redeploys...)
}

// fakeProbe 可设的探测结果：默认成功；fail 非 nil 时返回它。记录调用次数。
type fakeProbe struct {
	mu    sync.Mutex
	fail  error
	calls int
	keys  []string
}

func (p *fakeProbe) probe(_ context.Context, _ llm.Node, _ string, _ upstream.Target, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.keys = append(p.keys, key)
	return p.fail
}

func (p *fakeProbe) set(err error) {
	p.mu.Lock()
	p.fail = err
	p.mu.Unlock()
}

func (p *fakeProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

var errProbe = errors.New("connection refused")

// nodeEnv：env + 假 rental / apps / 探测 + Controller。
type nodeEnv struct {
	*env
	rental *fakeRental
	apps   *fakeApps
	probe  *fakeProbe
	ctl    *llm.Controller
}

func newNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()
	e := newEnv(t)
	ne := &nodeEnv{env: e, rental: &fakeRental{e: e}, apps: &fakeApps{e: e}, probe: &fakeProbe{}}
	e.svc = e.newService(func(d *llm.Deps) { d.Rental = ne.rental; d.Apps = ne.apps })
	ne.ctl = llm.NewController(e.svc)
	ne.ctl.Prober().SetProbe(ne.probe.probe)
	return ne
}

// tick 跑一轮 Controller（假时钟当前时刻），失败即 Fatal。
func (ne *nodeEnv) tick() llm.TickReport {
	ne.t.Helper()
	rep, err := ne.ctl.Tick(ne.ctx, ne.clk.Now())
	if err != nil {
		ne.t.Fatalf("Tick: %v", err)
	}
	return rep
}

// probeTick 跑一轮 Prober。
func (ne *nodeEnv) probeTick() {
	ne.t.Helper()
	if err := ne.ctl.Prober().Tick(ne.ctx, ne.clk.Now()); err != nil {
		ne.t.Fatalf("Prober.Tick: %v", err)
	}
}

func (ne *nodeEnv) node(id string) llm.Node {
	ne.t.Helper()
	d, err := ne.svc.GetNode(ne.ctx, id)
	if err != nil {
		ne.t.Fatalf("GetNode %s: %v", id, err)
	}
	return d.Node.Node
}

// nodesOf 某模型的节点（非 removed），按创建顺序。
func (ne *nodeEnv) nodesOf(modelID int64) []llm.NodeRow {
	ne.t.Helper()
	rows, err := ne.svc.ListNodes(ne.ctx, llm.NodeFilter{ModelID: modelID})
	if err != nil {
		ne.t.Fatal(err)
	}
	return rows
}

func (ne *nodeEnv) countState(modelID int64, state string) int {
	n := 0
	for _, r := range ne.nodesOf(modelID) {
		if r.State == state {
			n++
		}
	}
	return n
}

// bringReady 让模型的全部 provisioning 节点走完：实例 running → Create → 部署 running/ok → 探测成功 → ready。
func (ne *nodeEnv) bringReady(modelID int64) []llm.NodeRow {
	ne.t.Helper()
	for _, r := range ne.nodesOf(modelID) {
		if r.State == llm.NodeProvisioning {
			ne.setInstance(*r.InstanceID, rental.StateRunning)
		}
	}
	ne.tick() // Create
	for _, r := range ne.nodesOf(modelID) {
		if r.State == llm.NodeProvisioning {
			if r.DeploymentID == nil {
				ne.t.Fatalf("node %s has no deployment after tick", r.ID)
			}
			ne.setDeployment(*r.DeploymentID, appdeploy.StateRunning, appdeploy.HealthOK, "")
		}
	}
	ne.probe.set(nil)
	ne.tick() // 探测成功 → ready
	return ne.nodesOf(modelID)
}

// setInstance 改实例状态；released / failed / cancelled 时同时结束 lease、机器回 idle（模拟洗机完成）。
func (e *env) setInstance(id, state string) {
	e.t.Helper()
	e.exec(`UPDATE instances SET state = $2 WHERE id = $1`, id, state)
	if state == rental.StateReleased || state == rental.StateFailed || state == rental.StateCancelled {
		e.exec(`UPDATE machine_leases SET status = 'ended' WHERE instance_id = $1 AND status IN ('reserved', 'active')`, id)
		e.exec(`UPDATE machines SET state = 'idle' WHERE id = (SELECT machine_id FROM instances WHERE id = $1)`, id)
	}
}

func (e *env) setDeployment(id, state, health, errCode string) {
	e.t.Helper()
	e.exec(`UPDATE app_deployments SET state = $2, health = $3, error_code = $4 WHERE id = $1`, id, state, health, errCode)
}

func (e *env) alertCount(kind string) int {
	e.t.Helper()
	return int(e.queryInt(`SELECT count(*) FROM alerts WHERE kind = $1`, kind))
}

func (e *env) eventStates(nodeID string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT from_state || '>' || to_state FROM llm_node_events WHERE node_id = $1 ORDER BY id`, nodeID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// rentalLease 在机器上造一个用户实例与 reserved / active lease（[from, until)），模拟「池里机器还有租约」。
func (e *env) rentalLease(machineID int64, state, leaseStatus string, from, until time.Time) string {
	e.t.Helper()
	id := randx.ID("ins-", 8)
	user := e.verifiedUser()
	e.exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		SELECT $1, $2, m.id, m.product_id, $3, $4, '\x00', $5, $5 FROM machines m WHERE m.id = $6`,
		id, user, e.otherImageID, state, e.clk.Now(), machineID)
	e.exec(`INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), $5, $6, $6)`, machineID, id, from, until, leaseStatus, e.clk.Now())
	return id
}
