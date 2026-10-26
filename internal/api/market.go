package api

import (
	"net/http"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/rental"
)

type productView struct {
	catalog.ProductWithPrices
	IdleCount int `json:"idle_count"`
	// 计划 13：此刻可组成的集群数，键为台数（"2" / "4"）
	Clusters map[string]rental.ClusterAvail `json:"clusters"`
}

func (s *Server) handleProducts(w http.ResponseWriter, r *http.Request) {
	products, err := s.Catalog.ListProducts(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]productView, 0, len(products))
	for _, p := range products {
		n, err := s.Rental.AvailableNow(r.Context(), p.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		cl, err := s.productClusters(r.Context(), p.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		views = append(views, productView{ProductWithPrices: p, IdleCount: n, Clusters: cl})
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
