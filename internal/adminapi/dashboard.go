package adminapi

import (
	"context"
	"math"
	"net/http"
	"time"

	"spark-cloud/internal/platform/httpx"
)

type dayRate struct {
	Date string  `json:"date"`
	Rate float64 `json:"rate"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.Clock.Now()
	local := now.In(cst)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, cst)
	month := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, cst)
	tomorrow, nextMonth := today.AddDate(0, 0, 1), month.AddDate(0, 1, 0)

	machines, err := s.Fleet.Stats(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	instances, err := s.Rental.InstanceStateCounts(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	sshOpen, err := s.Rental.OpenSSHSessionCount(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var firstErr error
	sum := func(f func(context.Context, time.Time, time.Time) (int64, error), from, to time.Time) int64 {
		if firstErr != nil {
			return 0
		}
		v, err := f(ctx, from, to)
		if err != nil {
			firstErr = err
		}
		return v
	}
	rechargeToday := sum(s.Billing.PaidAmount, today, tomorrow)
	rechargeMonth := sum(s.Billing.PaidAmount, month, nextMonth)
	orderToday := sum(s.Rental.OrderAmount, today, tomorrow)
	orderMonth := sum(s.Rental.OrderAmount, month, nextMonth)
	if firstErr != nil {
		s.writeError(w, r, firstErr)
		return
	}
	const days = 30
	from := today.AddDate(0, 0, -(days - 1))
	occ, err := s.Rental.DailyOccupancy(ctx, from, days, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	util := make([]dayRate, len(occ))
	for i, secs := range occ {
		day := from.AddDate(0, 0, i)
		span := 86400.0
		if i == len(occ)-1 {
			span = now.Sub(day).Seconds()
		}
		rate := 0.0
		if machines.Total > 0 && span > 0 {
			rate = math.Min(1, secs/(float64(machines.Total)*span))
		}
		util[i] = dayRate{Date: day.Format("2006-01-02"), Rate: math.Round(rate*10000) / 10000}
	}
	upcoming, err := s.upcoming(r, 7)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	refMonth, err := s.Referral.MonthSettledNet(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	upcoming7 := 0
	for _, d := range upcoming {
		upcoming7 += d.Count
	}
	appsInstalling, appsRunning, appsFailed24h, err := s.Apps.Counts(ctx, now)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var wfInstalls, wfRuns24h int
	if s.Workflows != nil {
		if wfInstalls, wfRuns24h, err = s.Workflows.AdminCounts(ctx, now); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	clusters, err := s.Rental.AdminClusterCounts(ctx) // 计划 13：集群卡片「活跃 N（就绪 / 降级）」
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := map[string]any{
		"machines": machines, "instances": instances, "clusters": clusters,
		"recharge_today": rechargeToday, "recharge_month": rechargeMonth, "order_today": orderToday, "order_month": orderMonth,
		"utilization": util, "ssh_sessions_open": sshOpen, "bookings_upcoming_7d": upcoming7,
		"referral_month_settled": refMonth,
		"apps_installing":        appsInstalling, "apps_running": appsRunning, "apps_failed_24h": appsFailed24h,
		"workflow_installs": wfInstalls, "workflow_runs_24h": wfRuns24h,
	}
	if s.LLM != nil { // 计划 9：Token 商城卡片（= /llm/summary 的子集）
		sum, err := s.LLM.AdminSummary(ctx)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		out["llm"] = map[string]any{"nodes_ready": sum.Nodes.Ready, "nodes_total": sum.Nodes.Total, "nodes_unhealthy": sum.Nodes.Unhealthy,
			"today_requests": sum.Today.Requests, "today_settled_amount": sum.Today.SettledAmount}
	}
	if err := s.addButlerDashboard(ctx, now, out); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
