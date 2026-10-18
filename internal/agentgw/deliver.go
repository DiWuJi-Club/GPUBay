package agentgw

import (
	"context"
	"time"
)

func (s *Server) runTicks(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ticks.Add(1) // Task 10 在此处执行投递
		}
	}
}
