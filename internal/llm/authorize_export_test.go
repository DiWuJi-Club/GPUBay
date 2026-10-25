package llm

// SetAuthorizeAfterLoadHook 仅测试：Authorize 查库之后、写缓存之前调用 f（模拟并发失效的竞态窗口）。
func SetAuthorizeAfterLoadHook(s *Service, f func()) { s.keyCache.afterLoad = f }
