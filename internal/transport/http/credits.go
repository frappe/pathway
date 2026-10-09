package http

import (
	"net/http"

	"github.com/phot0n/pathway/internal/transport/respond"
)

const nanoPerUSD = 1e9

// creditsBody is GET /v1/credits, in US dollars: what this key has left of its cap on this
// store. The names are the control plane's own (grove.api.balance), so a holder reads one shape
// from either side.
type creditsBody struct {
	Balance    float64 `json:"balance"`
	Spent      float64 `json:"spent"`
	IsFreeUser bool    `json:"is_free_user"`
}

// handleCredits answers what the key has left on this store — the figure the quota stage gates
// on. Its own cap and nothing of the team's, so every key may read it. An exhausted key still
// reads it: that is when it is asked. A free one is never charged, so there is nothing to report
// but that.
func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.identifyCaller(w, r)
	if !ok {
		return
	}
	key := identity.Key
	if !key.Prepaid {
		respond.JSON(w, creditsBody{IsFreeUser: true})
		return
	}
	respond.JSON(w, creditsBody{
		Balance: float64(key.Budget-key.Spent) / nanoPerUSD,
		Spent:   float64(key.Spent) / nanoPerUSD,
	})
}
