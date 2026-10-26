package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// ProbeFunc 对一个节点做一次 HTTP 健康探测：nil = 成功。slug 是节点所属模型的 slug（vLLM --served-model-name）。
type ProbeFunc func(ctx context.Context, node Node, slug string, target upstream.Target, key string) error

// MaxProbeConcurrency 一轮探测的并发上限（规格 §5.3）。
const MaxProbeConcurrency = 16

// maxProbeBody /v1/models 响应体读取上限。
const maxProbeBody = 1 << 20

// Prober 节点健康探测（规格 §5.3；组节点见 observeGroup）：对 ready / unhealthy / provisioning（部署 running）节点经 upstream 拨号
// GET /v1/models。探测在事务外；回写只锁该节点行（mutateNode）。
type Prober struct {
	s     *Service
	probe ProbeFunc
}

// NewProber 默认探测函数 = DefaultProbe（经 Deps.Upstream 拨号）。
func NewProber(s *Service) *Prober {
	p := &Prober{s: s}
	p.probe = s.DefaultProbe
	return p
}

// SetProbe 替换探测函数（测试注入；e2e 仍用默认的 DefaultProbe 打 FakeVLLM）。
func (p *Prober) SetProbe(f ProbeFunc) { p.probe = f }

// DefaultProbe 经 Deps.Upstream.Dial(target, NodePort) 发 GET /v1/models（Authorization: Bearer <节点密钥>，ProbeTimeout）；
// 2xx 且 data[].id 含 slug → 成功。
func (s *Service) DefaultProbe(ctx context.Context, _ Node, slug string, target upstream.Target, key string) error {
	if s.d.Upstream == nil {
		return errors.New("upstream pool not configured")
	}
	return probeModels(ctx, func(ctx context.Context) (net.Conn, error) { return s.d.Upstream.Dial(ctx, target, NodePort) }, slug, key)
}

// probeModels 是 DefaultProbe 的 HTTP 部分（dial 可替换，便于单测）。
func probeModels(ctx context.Context, dial func(context.Context) (net.Conn, error), slug, key string) error {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProbeBody)).Decode(&body); err != nil {
		return fmt.Errorf("bad /v1/models response: %w", err)
	}
	for _, d := range body.Data {
		if d.ID == slug {
			return nil
		}
	}
	return fmt.Errorf("model %s not served", slug)
}

// verdict 一次观察的结论。
type verdict int

const (
	vSkip    verdict = iota // 不下结论（读库失败、provisioning 尚未就绪）：不回写
	vOK                     // 探测成功
	vFail                   // 计一次失败
	vFailNow                // 立即 unhealthy（部署 failed / 实例已结束）
)

type observation struct {
	v      verdict
	detail string
}

// observe 读实例 / 部署状态并（必要时）探测。provisioning 只在部署 running 且 health = ok 时才探测，其余 vSkip。
func (p *Prober) observe(ctx context.Context, n Node, slug string) observation {
	if n.Size > 1 {
		return p.observeGroup(ctx, n, slug)
	}
	s := p.s
	prov := n.State == NodeProvisioning
	if n.InstanceID == nil {
		return observation{vFailNow, "节点没有实例"}
	}
	inst, err := s.d.Rental.Get(ctx, *n.InstanceID)
	if errors.Is(err, apperr.ErrNotFound) {
		return observation{vFailNow, "实例不存在"}
	}
	if err != nil {
		s.d.Log.Warn("llm prober: read instance", "node", n.ID, "err", err)
		return observation{vSkip, ""}
	}
	switch {
	case instanceGone(inst.State):
		return observation{vFailNow, "实例已结束（" + inst.State + "）"}
	case inst.State != rental.StateRunning:
		if prov {
			return observation{vSkip, ""}
		}
		return observation{vFail, "实例未运行（" + inst.State + "）"}
	case inst.SuspendedAt != nil:
		if prov {
			return observation{vSkip, ""}
		}
		return observation{vFail, "机器失联，实例已暂停"}
	}
	// 没有部署：provisioning 等 Controller 建部署；ready / unhealthy 只可能是 e2e 钩子直接造的节点（SeedReadyNode）——
	// 跳过部署检查，直接做 HTTP 探测（生产里 ready 节点一定有部署，此分支不会走到）
	if n.DeploymentID == nil && prov {
		return observation{vSkip, ""}
	}
	if n.DeploymentID != nil {
		if obs, done := p.observeDeployment(ctx, n, prov); done {
			return obs
		}
	}
	return p.probeNode(ctx, n, slug)
}

// observeDeployment 部署状态检查；done 为真时 obs 即结论，否则继续 HTTP 探测。
func (p *Prober) observeDeployment(ctx context.Context, n Node, prov bool) (obs observation, done bool) {
	s := p.s
	dep, err := s.d.Apps.Get(ctx, *n.DeploymentID)
	if err != nil {
		s.d.Log.Warn("llm prober: read deployment", "node", n.ID, "err", err)
		return observation{vSkip, ""}, true
	}
	switch {
	case dep.State == appdeploy.StateFailed || dep.State == appdeploy.StateRemoved || dep.State == appdeploy.StateLost:
		if prov {
			return observation{vSkip, ""}, true // Controller 第 2 步负责 provisioning → failed
		}
		return observation{vFailNow, deploymentError(dep)}, true
	case dep.State != appdeploy.StateRunning:
		if prov {
			return observation{vSkip, ""}, true
		}
		return observation{vFail, "部署未运行（" + dep.State + "）"}, true
	case dep.Health == appdeploy.HealthUnhealthy:
		if prov {
			return observation{vSkip, ""}, true
		}
		return observation{vFail, "部署健康检查未通过"}, true
	case prov && dep.Health != appdeploy.HealthOK:
		return observation{vSkip, ""}, true
	}
	return observation{}, false
}

// probeNode 经上游拨号做一次 HTTP 探测（GET /v1/models，节点密钥）。
func (p *Prober) probeNode(ctx context.Context, n Node, slug string) observation {
	s := p.s
	target, err := s.d.Rental.UpstreamTarget(ctx, *n.InstanceID)
	if err != nil {
		return observation{vFail, "读取拨号目标失败"}
	}
	key, err := s.d.Box.Open(n.APIKeyEnc)
	if err != nil {
		s.d.Log.Error("llm prober: open node key", "node", n.ID, "err", err)
		return observation{vSkip, ""}
	}
	if err := p.probe(ctx, n, slug, target, string(key)); err != nil {
		return observation{vFail, clipRunes("健康检查失败："+err.Error(), maxNodeError)}
	}
	return observation{vOK, ""}
}

// deploymentError 部署失败的可展示原因：错误码中文 + message。
func deploymentError(d appdeploy.Deployment) string {
	text := appdeploy.ErrorText[d.ErrorCode]
	if text == "" {
		text = "部署" + d.State
	}
	if d.Message != "" {
		text += "：" + d.Message
	}
	return clipRunes(text, maxNodeError)
}

// unhealthyAlert 节点转 unhealthy 的告警文案（稳定，不含数值）。
func unhealthyAlert(n Node, slug string) fleet.Alert {
	inst := ""
	if n.InstanceID != nil {
		inst = *n.InstanceID
	}
	return fleet.Alert{Level: "warning", Kind: AlertNodeUnhealthy, MachineID: n.MachineID, InstanceID: inst,
		Message: "节点 " + n.ID + "（" + slug + "）健康检查失败"}
}

// probeNode 一行待探测节点。
type probeNode struct {
	Node
	slug string
}

// Tick 一轮探测（规格 §5.3），并发 ≤ MaxProbeConcurrency。单节点的读写失败记日志、合并进返回的错误。
func (p *Prober) Tick(ctx context.Context, now time.Time) error {
	rows, err := p.s.d.Pool.Query(ctx, `SELECT `+nodeColsN+`, md.slug
		FROM llm_nodes n JOIN llm_models md ON md.id = n.model_id
		WHERE n.state IN ($1, $2) OR (n.state = $3 AND (n.deployment_id IS NOT NULL OR n.app_cluster_id IS NOT NULL)) ORDER BY n.id`,
		NodeReady, NodeUnhealthy, NodeProvisioning)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (probeNode, error) {
		var x probeNode
		err := r.Scan(nodeDest(&x.Node, &x.slug)...)
		return x, err
	})
	if err != nil {
		return err
	}
	sem := make(chan struct{}, MaxProbeConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, pn := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(pn probeNode) {
			defer func() { <-sem; wg.Done() }()
			var err error
			if pn.State == NodeProvisioning {
				_, err = p.tryPromote(ctx, pn.Node, pn.slug)
			} else {
				err = p.checkLive(ctx, pn.Node, pn.slug)
			}
			if err != nil {
				p.s.d.Log.Error("llm prober: node", "node", pn.ID, "err", err)
				mu.Lock()
				errs = append(errs, fmt.Errorf("node %s: %w", pn.ID, err))
				mu.Unlock()
			}
		}(pn)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// checkLive ready / unhealthy 节点：ready 连续 ProbeFailThreshold 次失败（或一次立即失败）→ unhealthy + 告警（一次）；
// unhealthy 一次成功 → ready。每次观察写 last_probe_at。
func (p *Prober) checkLive(ctx context.Context, n0 Node, slug string) error {
	obs := p.observe(ctx, n0, slug)
	if obs.v == vSkip {
		return nil
	}
	_, _, err := p.s.mutateNode(ctx, n0.ID, func(tx pgx.Tx, n *Node, now time.Time) (string, error) {
		if err := expect(n, n0.State); err != nil {
			return "", err
		}
		n.LastProbeAt = &now
		if obs.v == vOK {
			n.ProbeFailCount, n.Error = 0, ""
			if n.State == NodeUnhealthy {
				n.State, n.UnhealthySince, n.LastReadyAt, n.Error = NodeReady, nil, &now, ""
				return "健康检查恢复", nil
			}
			return "", nil
		}
		n.ProbeFailCount++
		n.Error = obs.detail
		if n.State == NodeReady && (obs.v == vFailNow || n.ProbeFailCount >= ProbeFailThreshold) {
			n.State, n.UnhealthySince = NodeUnhealthy, &now
			return obs.detail, p.s.d.Fleet.RaiseAlertOnce(ctx, tx, unhealthyAlert(*n, slug))
		}
		return "", nil
	})
	return err
}

// tryPromote provisioning 节点：部署 running 且 health = ok 且探测成功 → ready（last_ready_at = now、probe_fail_count = 0）。
// 探测失败只写 last_probe_at（provisioning 的失败由超时兜底）。返回是否转 ready。
func (p *Prober) tryPromote(ctx context.Context, n0 Node, slug string) (bool, error) {
	obs := p.observe(ctx, n0, slug)
	if obs.v == vSkip {
		return false, nil
	}
	n, ok, err := p.s.mutateNode(ctx, n0.ID, func(tx pgx.Tx, n *Node, now time.Time) (string, error) {
		if err := expect(n, NodeProvisioning); err != nil {
			return "", err
		}
		n.LastProbeAt = &now
		if obs.v != vOK {
			n.Error = obs.detail
			return "", nil
		}
		n.State, n.ProbeFailCount, n.UnhealthySince, n.LastReadyAt, n.Error = NodeReady, 0, nil, &now, ""
		return "部署就绪，健康检查通过", nil
	})
	return ok && err == nil && n.State == NodeReady, err
}
