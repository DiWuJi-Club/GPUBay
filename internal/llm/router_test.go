package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/platform/dbtest"
)

// 同一 Key 优先上次的节点，直到其在途达到 0.8 × max；之后换最少在途的。
func TestRouterPrefersLastNodeUntil80Percent(t *testing.T) {
	a, b := rtNodeID(1), rtNodeID(2)
	r := rtNewRouter(rtNode(a, 1, 10), rtNode(b, 1, 10))
	const key = 7
	// 没有记忆：最少在途（并列按 ID）→ a；让 b 更空也会被选
	held := rtHold(r, a, 1)
	if got := rtMustPick(t, r, 1, key, nil); got != b {
		t.Fatalf("no memory: got %s, want least-inflight %s", got, b)
	}
	r.Remember(key, a)
	// a 在途 1..7（< 8 = 0.8×10）：一直选 a，即使 b 更空
	for n := 1; n <= 7; n++ {
		if got := rtMustPick(t, r, 1, key, nil); got != a {
			t.Fatalf("a inflight %d: got %s, want sticky %s", n, got, a)
		}
		if n < 7 {
			held = append(held, r.Acquire(a))
		}
	}
	if r.Inflight(a) != 7 {
		t.Fatalf("inflight(a) = %d", r.Inflight(a))
	}
	// a 在途 8 = 80%：换到最少在途的 b
	held = append(held, r.Acquire(a))
	if got := rtMustPick(t, r, 1, key, nil); got != b {
		t.Fatalf("a at 80%%: got %s, want %s", got, b)
	}
	// 另一个 Key 不受 key 的记忆影响
	if got := rtMustPick(t, r, 1, key+1, nil); got != b {
		t.Fatalf("other key: got %s", got)
	}
	// 释放一个 → a 回到 7：重新粘回 a
	held[len(held)-1]()
	held = held[:len(held)-1]
	if got := rtMustPick(t, r, 1, key, nil); got != a {
		t.Fatalf("a back under 80%%: got %s", got)
	}
	// 排除 a（失败转移）→ b
	if got := rtMustPick(t, r, 1, key, map[string]bool{a: true}); got != b {
		t.Fatalf("exclude a: got %s", got)
	}
	// 记住 b 后粘 b
	r.Remember(key, b)
	if got := rtMustPick(t, r, 1, key, nil); got != b {
		t.Fatalf("remember b: got %s", got)
	}
	// 记不在节点表里的节点：忽略，记忆仍是 b
	r.Remember(key, rtNodeID(99))
	if got := rtMustPick(t, r, 1, key, nil); got != b {
		t.Fatalf("unknown node remembered: got %s", got)
	}
	// 同一 Key 在另一个模型上的记忆互不覆盖
	r.setNodes([]RouteNode{rtNode(a, 1, 10), rtNode(b, 1, 10), rtNode(rtNodeID(5), 9, 10), rtNode(rtNodeID(6), 9, 10)})
	r.Remember(key, rtNodeID(6))
	if got := rtMustPick(t, r, 9, key, nil); got != rtNodeID(6) {
		t.Fatalf("model 9 sticky: got %s", got)
	}
	if got := rtMustPick(t, r, 1, key, nil); got != b {
		t.Fatalf("model 1 memory overwritten: got %s", got)
	}
	// 记忆里的节点掉出节点表（非 ready）→ 记忆清除，回到最少在途（a 在途 7、b 0）
	r.setNodes([]RouteNode{rtNode(a, 1, 10), rtNode(rtNodeID(5), 9, 10)})
	if got := rtMustPick(t, r, 1, key, nil); got != a {
		t.Fatalf("stale memory: got %s", got)
	}
	r.setNodes([]RouteNode{rtNode(a, 1, 10), rtNode(b, 1, 10)})
	if got := rtMustPick(t, r, 1, key, nil); got != b {
		t.Fatalf("memory of dropped node must be gone: got %s", got)
	}
	// max=1 的节点：在途 0 < 0.8 仍可粘
	c := rtNodeID(3)
	r2 := rtNewRouter(rtNode(c, 2, 1), rtNode(rtNodeID(4), 2, 1))
	r2.Remember(key, rtNodeID(4))
	if got := rtMustPick(t, r2, 2, key, nil); got != rtNodeID(4) {
		t.Fatalf("max=1 sticky: got %s", got)
	}
	rtReleaseAll(held)
	if r.Inflight(a) != 0 || r.Inflight(b) != 0 {
		t.Fatalf("inflight not back to 0: a=%d b=%d", r.Inflight(a), r.Inflight(b))
	}
}

// 最少在途优先、只看本模型的节点、exclude 生效；全部排除 → ErrNoNode。
func TestRouterLeastInflightAndExclude(t *testing.T) {
	n1, n2, n3, other := rtNodeID(1), rtNodeID(2), rtNodeID(3), rtNodeID(4)
	r := rtNewRouter(rtNode(n1, 1, 8), rtNode(n2, 1, 8), rtNode(n3, 1, 8), rtNode(other, 2, 8))
	held := append(rtHold(r, n1, 3), rtHold(r, n2, 1)...)
	held = append(held, rtHold(r, n3, 2)...)
	if got := rtMustPick(t, r, 1, 1, nil); got != n2 {
		t.Fatalf("least inflight: got %s, want %s", got, n2)
	}
	if got := rtMustPick(t, r, 1, 1, map[string]bool{n2: true}); got != n3 {
		t.Fatalf("exclude n2: got %s, want %s", got, n3)
	}
	if got := rtMustPick(t, r, 1, 1, map[string]bool{n2: true, n3: true}); got != n1 {
		t.Fatalf("exclude n2,n3: got %s", got)
	}
	if _, err := r.Pick(1, 1, map[string]bool{n1: true, n2: true, n3: true}); !errors.Is(err, ErrNoNode) {
		t.Fatalf("all excluded: %v", err)
	}
	// 模型 2 只有 other；模型 3 没有节点
	if got := rtMustPick(t, r, 2, 1, nil); got != other {
		t.Fatalf("model 2: got %s", got)
	}
	if _, err := r.Pick(3, 1, nil); !errors.Is(err, ErrNoNode) {
		t.Fatalf("model 3: %v", err)
	}
	// 选出的节点带完整 Target
	n, _ := r.Pick(1, 1, nil)
	if n.Target.InstanceID != "ins-"+n2 || n.MaxConcurrency != 8 {
		t.Fatalf("route node = %+v", n)
	}
	rtReleaseAll(held)

	// -race：并发 Pick / Acquire / release / Remember / 刷新节点表后计数回零
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				node, err := r.Pick(1, int64(g), nil)
				if err != nil {
					continue
				}
				rel := r.Acquire(node.ID)
				r.Remember(int64(g), node.ID)
				_ = r.Inflight(node.ID)
				rel()
				if g == 0 && i%50 == 0 {
					r.setNodes([]RouteNode{rtNode(n1, 1, 8), rtNode(n2, 1, 8), rtNode(n3, 1, 8), rtNode(other, 2, 8)})
				}
			}
		}(g)
	}
	wg.Wait()
	for _, id := range []string{n1, n2, n3, other} {
		if r.Inflight(id) != 0 {
			t.Fatalf("inflight(%s) = %d after concurrent use", id, r.Inflight(id))
		}
	}
}

// 没有 ready 节点 → ErrNoNode；有节点但全满 → ErrAllBusy；释放一个即恢复。
func TestRouterNoNodeVsAllBusy(t *testing.T) {
	r := rtNewRouter()
	if _, err := r.Pick(1, 1, nil); !errors.Is(err, ErrNoNode) {
		t.Fatalf("empty: %v", err)
	}
	a, b := rtNodeID(1), rtNodeID(2)
	r.setNodes([]RouteNode{rtNode(a, 1, 2), rtNode(b, 1, 3)})
	ha := rtHold(r, a, 2)
	hb := rtHold(r, b, 3)
	r.Remember(1, a)
	if _, err := r.Pick(1, 1, nil); !errors.Is(err, ErrAllBusy) {
		t.Fatalf("all full: %v", err)
	}
	// 排除掉一个满节点，剩下的也满 → 仍是 ErrAllBusy（有节点只是忙）
	if _, err := r.Pick(1, 1, map[string]bool{a: true}); !errors.Is(err, ErrAllBusy) {
		t.Fatalf("one excluded, rest full: %v", err)
	}
	if errors.Is(ErrAllBusy, ErrNoNode) || errors.Is(ErrNoNode, ErrAllBusy) {
		t.Fatal("errors must be distinct")
	}
	hb[0]()
	if got := rtMustPick(t, r, 1, 1, nil); got != b {
		t.Fatalf("after release: got %s", got)
	}
	// release 幂等
	hb[0]()
	if r.Inflight(b) != 2 {
		t.Fatalf("double release: inflight(b) = %d", r.Inflight(b))
	}
	rtReleaseAll(ha)
	rtReleaseAll(hb[1:])
}

// Refresh 只装 ready 且模型 online 的节点（max_concurrency、lan_ip、主机公钥来自库）；
// 节点转 draining 后不再被选，但它的在途计数仍可读、请求结束照常归零。
func TestRouterRefreshDropsNonReady(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t0 := dbtest.T0
	img := dbtest.ImageID(t, pool)
	prod := dbtest.ProductID(t, pool)
	var sys int64
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE system AND phone = $1`, SystemUserPhone).Scan(&sys); err != nil {
		t.Fatal(err)
	}
	model := func(slug, status string, maxConc int) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, input_price, output_price, status, replicas, max_concurrency, created_at, updated_at)
			VALUES ($1, $1, 'chat', 'Qwen/x', $2, 32768, 100, 400, $3, 1, $4, $5, $5) RETURNING id`, slug, img, status, maxConc, t0).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	online := model("qwen3-4b", ModelOnline, 16)
	offline := model("qwen3-8b", ModelOffline, 32)
	node := func(i int, modelID int64, state, lanIP, pub string) string {
		t.Helper()
		m := dbtest.CreateMachine(t, pool, rtNodeID(i))
		exec(`UPDATE machines SET lan_ip = $2 WHERE id = $1`, m, lanIP)
		ins := "ins-rt" + rtNodeID(i)[len(rtNodeID(i))-6:]
		exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, system, ssh_host_pubkey, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'running', 'x', true, $6, $7, $7)`, ins, sys, m, prod, img, pub, t0)
		id := rtNodeID(i)
		exec(`INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, '\x00', $6, $6)`, id, modelID, m, ins, state, t0)
		return id
	}
	ready1 := node(1, online, NodeReady, "10.0.1.1", "ssh-ed25519 AAAA1")
	ready2 := node(2, online, NodeReady, "10.0.1.2", "")
	unhealthy := node(3, online, NodeUnhealthy, "10.0.1.3", "")
	_ = node(4, online, NodeProvisioning, "10.0.1.4", "")
	offReady := node(5, offline, NodeReady, "10.0.1.5", "")

	r := NewRouter(NewService(Deps{Pool: pool}))
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	got := r.byModel[online]
	_, hasOff := r.byModel[offline]
	_, hasUnhealthy := r.nodes[unhealthy]
	_, hasOffNode := r.nodes[offReady]
	r.mu.Unlock()
	if len(got) != 2 || got[0].ID != ready1 || got[1].ID != ready2 || hasOff || hasUnhealthy || hasOffNode {
		t.Fatalf("table = %+v (offline model present %v)", got, hasOff)
	}
	if got[0].MaxConcurrency != 16 || got[0].ModelID != online || got[0].Target.LanIP != "10.0.1.1" ||
		got[0].Target.HostPubkey != "ssh-ed25519 AAAA1" || got[0].Target.InstanceID != "ins-rt000001" {
		t.Fatalf("route node = %+v", got[0])
	}
	if _, err := r.Pick(offline, 1, nil); !errors.Is(err, ErrNoNode) {
		t.Fatalf("offline model: %v", err)
	}

	// ready1 有一个在途请求，随后被排空
	rel := r.Acquire(ready1)
	r.Remember(1, ready1)
	if got := rtMustPick(t, r, online, 1, nil); got != ready1 {
		t.Fatalf("sticky before drain: %s", got)
	}
	exec(`UPDATE llm_nodes SET state = 'draining' WHERE id = $1`, ready1)
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := rtMustPick(t, r, online, 1, nil); got != ready2 {
			t.Fatalf("draining node still picked: %s", got)
		}
	}
	if r.Inflight(ready1) != 1 {
		t.Fatalf("draining inflight = %d, want 1", r.Inflight(ready1))
	}
	// 再刷新一次计数也不丢
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if r.Inflight(ready1) != 1 {
		t.Fatalf("inflight lost across refresh: %d", r.Inflight(ready1))
	}
	rel()
	if r.Inflight(ready1) != 0 {
		t.Fatalf("after release = %d", r.Inflight(ready1))
	}
	r.mu.Lock()
	_, kept := r.inflight[ready1]
	_, remembered := r.lastNode[stickyKey{1, online}]
	r.mu.Unlock()
	if kept || remembered {
		t.Fatalf("stale state kept: counter %v, lastNode %v", kept, remembered)
	}

	// 全部不 ready → 节点表清空
	exec(`UPDATE llm_nodes SET state = 'unhealthy'`)
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Pick(online, 1, nil); !errors.Is(err, ErrNoNode) {
		t.Fatalf("no ready nodes: %v", err)
	}

	// Run：先刷新一次再按周期；ctx 取消即返回
	exec(`UPDATE llm_nodes SET state = 'ready' WHERE id = $1`, ready2)
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { r.Run(rctx, RouterRefresh); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := r.Pick(online, 1, nil); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatal("Run returned early")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Run did not refresh")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
}
