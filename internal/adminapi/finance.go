package adminapi

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

var cst = time.FixedZone("CST", 8*3600)

func (s *Server) financeRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/orders", financeRoles, s.handleListOrders)
	s.handle(g, http.MethodGet, "/orders/export", financeRoles, s.handleExportOrders)
	s.handle(g, http.MethodGet, "/ledger", financeRoles, s.handleListLedger)
	s.handle(g, http.MethodGet, "/ledger/export", financeRoles, s.handleExportLedger)
	s.handle(g, http.MethodGet, "/recharges", financeRoles, s.handleListRecharges)
	s.handle(g, http.MethodGet, "/recharges/export", financeRoles, s.handleExportRecharges)
}

func parseTimeParam(name, v string, endOfDay bool) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", v, cst); err == nil {
		if endOfDay {
			t = t.AddDate(0, 0, 1)
		}
		t = t.UTC()
		return &t, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		t = t.UTC()
		return &t, nil
	}
	return nil, apperr.BadRequest(name + " 的格式应为 2006-01-02 或 RFC3339")
}

// parseRange 解析 from / to：日期按北京时间，to 为日期时包含当天。
func parseRange(r *http.Request) (*time.Time, *time.Time, error) {
	q := r.URL.Query()
	from, err := parseTimeParam("from", q.Get("from"), false)
	if err != nil {
		return nil, nil, err
	}
	to, err := parseTimeParam("to", q.Get("to"), true)
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && !from.Before(*to) {
		return nil, nil, apperr.BadRequest("from 必须早于 to")
	}
	return from, to, nil
}

// csvExport 以附件形式输出 CSV。run 必须在写出第一行之前完成行数检查（超过上限直接返回错误，此时仍可返回 JSON 错误）。
func (s *Server) csvExport(w http.ResponseWriter, r *http.Request, name string, header []string,
	run func(write func([]string) error) error) {
	var cw *csvx.Writer
	start := func() error {
		if cw != nil {
			return nil
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.csv"`, name, s.Clock.Now().In(cst).Format("20060102-150405")))
		var err error
		if cw, err = csvx.NewWriter(w); err != nil {
			return err
		}
		return cw.Write(header)
	}
	err := run(func(rec []string) error {
		if err := start(); err != nil {
			return err
		}
		return cw.Write(rec)
	})
	if err == nil {
		err = start()
	}
	if err != nil {
		if cw == nil {
			s.writeError(w, r, err)
			return
		}
		s.Log.Error("csv export aborted", "name", name, "err", err)
		return
	}
	if err := cw.Flush(); err != nil {
		s.Log.Error("csv export flush", "name", name, "err", err)
	}
}

func idText(id *int64) string {
	if id == nil {
		return ""
	}
	return strconv.FormatInt(*id, 10)
}

func (s *Server) orderFilter(r *http.Request) (rental.OrderFilter, error) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return rental.OrderFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return rental.OrderFilter{}, err
	}
	q := r.URL.Query()
	return rental.OrderFilter{UserID: uid, Kind: q.Get("kind"), Status: q.Get("status"), From: from, To: to}, nil
}

func (s *Server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.orderFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListOrders(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(orderViews(list), total))
}

func (s *Server) handleExportOrders(w http.ResponseWriter, r *http.Request) {
	f, err := s.orderFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "orders", []string{"订单号", "用户ID", "类型", "实例", "租期", "金额(元)", "已退(元)", "状态", "下单时间"},
		func(write func([]string) error) error {
			return s.Rental.ExportOrders(r.Context(), f, func(o rental.Order) error {
				return write([]string{o.ID, strconv.FormatInt(o.UserID, 10), o.Kind, o.InstanceID, string(o.Duration),
					csvx.Yuan(o.Amount), csvx.Yuan(o.RefundedAmount), o.Status, csvx.Time(o.CreatedAt)})
			})
		})
}

func (s *Server) ledgerFilter(r *http.Request) (wallet.LedgerFilter, error) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return wallet.LedgerFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return wallet.LedgerFilter{}, err
	}
	return wallet.LedgerFilter{UserID: uid, Kind: r.URL.Query().Get("kind"), From: from, To: to}, nil
}

func (s *Server) handleListLedger(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.ledgerFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Wallet.AdminListLedger(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleExportLedger(w http.ResponseWriter, r *http.Request) {
	f, err := s.ledgerFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "ledger", []string{"流水ID", "用户ID", "类型", "金额(元)", "变动后余额(元)", "关联类型", "关联单号", "操作人", "备注", "时间"},
		func(write func([]string) error) error {
			return s.Wallet.ExportLedger(r.Context(), f, func(l wallet.AdminLedgerRow) error {
				return write([]string{strconv.FormatInt(l.ID, 10), strconv.FormatInt(l.UserID, 10), string(l.Kind),
					csvx.Yuan(l.Amount), csvx.Yuan(l.BalanceAfter), l.RefType, l.RefID, idText(l.OperatorID), l.Remark, csvx.Time(l.CreatedAt)})
			})
		})
}

func (s *Server) rechargeFilter(r *http.Request) (billing.RechargeFilter, error) {
	uid, err := queryInt(r, "user_id")
	if err != nil {
		return billing.RechargeFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return billing.RechargeFilter{}, err
	}
	q := r.URL.Query()
	return billing.RechargeFilter{UserID: uid, Channel: q.Get("channel"), Status: q.Get("status"), From: from, To: to}, nil
}

func (s *Server) handleListRecharges(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.rechargeFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Billing.AdminList(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleExportRecharges(w http.ResponseWriter, r *http.Request) {
	f, err := s.rechargeFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "recharges", []string{"充值单号", "用户ID", "渠道", "金额(元)", "状态", "渠道单号", "操作人", "备注", "创建时间", "支付时间", "关闭时间"},
		func(write func([]string) error) error {
			return s.Billing.Export(r.Context(), f, func(c billing.Recharge) error {
				return write([]string{c.ID, strconv.FormatInt(c.UserID, 10), c.Channel, csvx.Yuan(c.Amount), c.Status, c.ChannelTradeNo,
					idText(c.OperatorID), c.Remark, csvx.Time(c.CreatedAt), csvx.TimePtr(c.PaidAt), csvx.TimePtr(c.ClosedAt)})
			})
		})
}
