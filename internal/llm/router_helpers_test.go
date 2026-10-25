package llm

import (
	"fmt"
	"testing"

	"spark-cloud/internal/access/upstream"
)

// rtNode 造一个路由节点（Target 用节点 ID 派生，便于断言）。
func rtNode(id string, modelID int64, max int) RouteNode {
	return RouteNode{ID: id, ModelID: modelID, MaxConcurrency: max,
		Target: upstream.Target{InstanceID: "ins-" + id, LanIP: "10.0.0.1"}}
}

// rtNewRouter 返回一个不连库的 Router，节点表直接装入 nodes。
func rtNewRouter(nodes ...RouteNode) *Router {
	r := NewRouter(NewService(Deps{}))
	r.setNodes(nodes)
	return r
}

// rtHold 在节点上占 n 个在途，返回全部释放函数。
func rtHold(r *Router, nodeID string, n int) []func() {
	out := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.Acquire(nodeID))
	}
	return out
}

func rtReleaseAll(rels []func()) {
	for _, f := range rels {
		f()
	}
}

func rtMustPick(t *testing.T, r *Router, modelID, keyID int64, exclude map[string]bool) string {
	t.Helper()
	n, err := r.Pick(modelID, keyID, exclude)
	if err != nil {
		t.Fatalf("Pick(%d, %d, %v): %v", modelID, keyID, exclude, err)
	}
	if n.ModelID != modelID {
		t.Fatalf("picked node %s of model %d, want model %d", n.ID, n.ModelID, modelID)
	}
	return n.ID
}

func rtNodeID(i int) string { return fmt.Sprintf("node-%012d", i) }
