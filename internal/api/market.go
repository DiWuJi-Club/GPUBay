package api

import (
	"net/http"

	"spark-cloud/internal/catalog"
)

type productView struct {
	catalog.ProductWithPrices
	IdleCount int `json:"idle_count"`
}

func (s *Server) handleProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.Catalog.ListProducts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]productView, 0, len(products))
	for _, p := range products {
		n, err := s.Fleet.IdleCount(r.Context(), p.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		views = append(views, productView{ProductWithPrices: p, IdleCount: n})
	}
	writeJSON(w, http.StatusOK, list(views))
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.Catalog.ListImages(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(images))
}
