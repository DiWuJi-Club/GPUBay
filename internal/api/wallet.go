package api

import (
	"net/http"
	"strconv"
)

func (s *Server) handleWallet(w http.ResponseWriter, r *http.Request) {
	bal, err := s.Wallet.Balance(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"balance": bal})
}

func (s *Server) handleLedger(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.Wallet.ListLedger(r.Context(), currentUser(r).ID, before, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(rows))
}
