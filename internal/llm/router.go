package llm

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/access/upstream"
)

var (
	// ErrNoNode：模型没有可用的 ready 节点（或都被 exclude 排除）→ 网关 503 model_unavailable。
	ErrNoNode = errors.New("llm: no ready node")
	// ErrAllBusy：有可用节点但在途都已达 max_concurrency → 网关 503 server_overloaded。
	ErrAllBusy = errors.New("llm: all nodes busy")
)

// RouteNode 是路由表的一行：ready 节点 + 模型的单节点并发上限 + 上游拨号目标（系统实例的 SSH）。
type RouteNode struct {
	ID             string
	ModelID        int64
	MaxConcurrency int
	Target         upstream.Target
}

// Router 是网关的进程内节点路由表（规格 §1.1 #9、§5.3 末条、§6.1 第 7 步）。
//
// 节点表每 RouterRefresh 从 llm_nodes 重建一次，只含 state='ready' 且模型 online 的节点。
// 在途计数与节点表相互独立：节点转 draining / 掉出节点表后，已在途请求的 release 照常递减，
// Controller 的排空检查经 Inflight(nodeID) 读到真实值；计数归零且节点不在表内时才回收。
// 进程内计数（T15）：多副本时各算一份。并发安全（单把锁；网关每秒几十次请求，锁内只做 O(节点数) 的扫描）。
type Router struct {
	s *Service

	mu       sync.Mutex
	byModel  map[int64][]RouteNode // 每个模型的 ready 节点（按 ID 升序）
	nodes    map[string]RouteNode
	inflight map[string]int       // nodeID → 在途请求数
	lastNode map[stickyKey]string // (Key, 模型) → 上次成功使用的节点（vLLM 前缀缓存亲和）
}

// stickyKey：同一 Key 调多个模型时各记各的节点。
type stickyKey struct{ keyID, modelID int64 }

func NewRouter(s *Service) *Router {
	return &Router{s: s, byModel: map[int64][]RouteNode{}, nodes: map[string]RouteNode{},
		inflight: map[string]int{}, lastNode: map[stickyKey]string{}}
}

// Refresh 从库里重建节点表：ready 节点 JOIN 模型（online、max_concurrency）JOIN 系统实例（running、未挂起）/ 机器（lan_ip、主机公钥）。
func (r *Router) Refresh(ctx context.Context) error {
	rows, err := r.s.d.Pool.Query(ctx, `SELECT n.id, n.model_id, md.max_concurrency, i.id, m.lan_ip, i.ssh_host_pubkey
		FROM llm_nodes n
		JOIN llm_models md ON md.id = n.model_id
		JOIN instances i ON i.id = n.instance_id
		JOIN machines m ON m.id = i.machine_id
		WHERE n.state = $1 AND md.status = $2 AND i.state = 'running' AND i.suspended_at IS NULL
		ORDER BY n.model_id, n.id`, NodeReady, ModelOnline)
	if err != nil {
		return err
	}
	nodes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (RouteNode, error) {
		var x RouteNode
		if err := row.Scan(&x.ID, &x.ModelID, &x.MaxConcurrency, &x.Target.InstanceID, &x.Target.LanIP, &x.Target.HostPubkey); err != nil {
			return x, err
		}
		if r.s.d.Rental != nil { // 计划 10 按实例端口：与 rental.UpstreamTarget（探测用）一致
			x.Target.Port = r.s.d.Rental.InstanceSSHPort(x.Target.InstanceID)
		}
		return x, nil
	})
	if err != nil {
		return err
	}
	r.setNodes(nodes)
	return nil
}

// setNodes 换上新节点表（nodes 须按 model_id、id 排序）；回收不在新表里的零在途计数与 lastNode。
func (r *Router) setNodes(nodes []RouteNode) {
	byModel := map[int64][]RouteNode{}
	byID := make(map[string]RouteNode, len(nodes))
	for _, n := range nodes {
		byModel[n.ModelID] = append(byModel[n.ModelID], n)
		byID[n.ID] = n
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byModel, r.nodes = byModel, byID
	for id, c := range r.inflight {
		if _, ok := byID[id]; !ok && c == 0 {
			delete(r.inflight, id)
		}
	}
	for k, id := range r.lastNode {
		if _, ok := byID[id]; !ok {
			delete(r.lastNode, k)
		}
	}
}

// Run 按 every 刷新直到 ctx 结束（先立即刷新一次）；刷新失败记日志并保留旧表。
func (r *Router) Run(ctx context.Context, every time.Duration) {
	refresh := func() {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := r.Refresh(c); err != nil && ctx.Err() == nil {
			r.s.d.Log.Error("llm router refresh failed; keeping previous table", "err", err)
		}
	}
	refresh()
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			refresh()
		}
	}
}

// Pick 为 (模型, Key) 选节点：lastNode[(keyID, modelID)] 在表内、未排除且在途 < 0.8 × max → 它；
// 否则未排除节点中在途最少且 < max 的（并列取 ID 小的）。
// 没有未排除的 ready 节点 → ErrNoNode；有但全满 → ErrAllBusy。exclude 用于首字节前的失败转移。
// Pick 不占在途：调用方随后 Acquire(node.ID)；网关应优先用 PickAcquire（选与占在同一把锁内，突发时不超 max）。
func (r *Router) Pick(modelID, keyID int64, exclude map[string]bool) (RouteNode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pickLocked(modelID, keyID, exclude)
}

// PickAcquire 同 Pick，但在同一把锁内给选中节点在途 +1，返回 release（−1，幂等）。
func (r *Router) PickAcquire(modelID, keyID int64, exclude map[string]bool) (RouteNode, func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.pickLocked(modelID, keyID, exclude)
	if err != nil {
		return RouteNode{}, nil, err
	}
	r.inflight[n.ID]++
	return n, r.releaser(n.ID), nil
}

// pickLocked 是 Pick 的策略本体；调用方持 r.mu。
func (r *Router) pickLocked(modelID, keyID int64, exclude map[string]bool) (RouteNode, error) {
	if id, ok := r.lastNode[stickyKey{keyID, modelID}]; ok && !exclude[id] {
		if n, ok := r.nodes[id]; ok && r.inflight[id]*5 < n.MaxConcurrency*4 {
			return n, nil
		}
	}
	var best RouteNode
	bestIn, candidates := -1, 0
	for _, n := range r.byModel[modelID] {
		if exclude[n.ID] {
			continue
		}
		candidates++
		in := r.inflight[n.ID]
		if in >= n.MaxConcurrency {
			continue
		}
		if bestIn < 0 || in < bestIn {
			best, bestIn = n, in
		}
	}
	switch {
	case bestIn >= 0:
		return best, nil
	case candidates == 0:
		return RouteNode{}, ErrNoNode
	default:
		return RouteNode{}, ErrAllBusy
	}
}

// Acquire 节点在途 +1，返回 release（−1，幂等）。节点不在表内（已排空）也照常计数。
func (r *Router) Acquire(nodeID string) (release func()) {
	r.mu.Lock()
	r.inflight[nodeID]++
	r.mu.Unlock()
	return r.releaser(nodeID)
}

// releaser 返回节点在途 −1 的幂等函数；归零且节点已不在表内时回收计数。
func (r *Router) releaser(nodeID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.inflight[nodeID]--
			if r.inflight[nodeID] <= 0 {
				if _, ok := r.nodes[nodeID]; !ok {
					delete(r.inflight, nodeID)
				}
			}
		})
	}
}

// Inflight 返回节点当前在途请求数（Controller 排空检查用）。
func (r *Router) Inflight(nodeID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inflight[nodeID]
}

// Remember 记下 Key 在该节点所属模型上最近成功使用的节点（请求完成后调用）；节点已不在表内则忽略。
func (r *Router) Remember(keyID int64, nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, ok := r.nodes[nodeID]; ok {
		r.lastNode[stickyKey{keyID, n.ModelID}] = nodeID
	}
}
