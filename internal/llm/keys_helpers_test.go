package llm_test

import (
	"fmt"
	"sync/atomic"

	"spark-cloud/internal/llm"
)

var keySeq atomic.Int64

// createKey 用 CreateKey 给用户建一把 Key（名称自动编号）；mods 在默认输入上改字段。返回明文与 Key。
func (e *env) createKey(userID int64, mods ...func(*llm.KeyInput)) (string, llm.Key) {
	e.t.Helper()
	in := llm.KeyInput{Name: fmt.Sprintf("key-%d", keySeq.Add(1))}
	for _, m := range mods {
		m(&in)
	}
	plain, k, err := e.svc.CreateKey(e.ctx, userID, in, "10.0.0.9")
	if err != nil {
		e.t.Fatalf("CreateKey: %v", err)
	}
	return plain, k
}

// authCode 调 Authorize，返回 *AuthError 的 HTTP 状态与 code（成功返回 0, ""）。
func (e *env) authCode(svc *llm.Service, plain string) (int, string) {
	e.t.Helper()
	_, err := svc.Authorize(e.ctx, llm.HashKey(plain), e.clk.Now())
	if err == nil {
		return 0, ""
	}
	ae, ok := err.(*llm.AuthError)
	if !ok {
		e.t.Fatalf("Authorize: non-AuthError %T %v", err, err)
	}
	return ae.Status, ae.Code
}
