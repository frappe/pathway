package http

import (
	"net/http"

	"github.com/phot0n/pathway/internal/transport/respond"
)

const nanoPerUSD = 1e9

// creditsBody is GET /v1/credits, in US dollars: what this key has left of its monthly cap on
// this store, this month. The names are the control plane's own, so a holder reads one shape
// from either side.
type creditsBody struct {
	Balance float64 `json:"balance"`
	Spent   float64 `json:"spent"`
}

// handleCredits answers what the key has left on this store this month — the figure the quota
// stage gates on. Its own cap and nothing of the team's, so every key may read it. An exhausted key still
// reads it: that is when it is asked. A free one is never charged, so it reads zeros.
func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.identifyCaller(w, r)
	if !ok {
		return
	}
	key := identity.Key
	if !key.Prepaid {
		respond.JSON(w, creditsBody{})
		return
	}
	respond.JSON(w, creditsBody{
		Balance: float64(key.Budget-key.Spent) / nanoPerUSD,
		Spent:   float64(key.Spent) / nanoPerUSD,
	})
}
