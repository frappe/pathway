package http

import (
	"net/http"

	"github.com/phot0n/pathway/internal/transport/respond"
)

const nanoPerUSD = 1e9

// creditsBody is GET /v1/credits, in US dollars. The names are the control plane's own
// (grove.api.balance), so a holder reads one shape from either side.
type creditsBody struct {
	Balance    float64 `json:"balance"`
	Spent      float64 `json:"spent"`
	IsFreeUser bool    `json:"is_free_user"`
}

// handleCredits answers what the holder has left on this store — the figure the quota stage gates
// on — to a key allowed to read it. An exhausted holder still reads it: that is when it is asked.
// A free holder is never charged, so there is nothing to report but that.
func (s *Server) handleCredits(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.identifyCaller(w, r)
	if !ok {
		return
	}
	if !identity.Key.CanReadBalance {
		respond.ErrorFor(w, r, http.StatusForbidden, "this key cannot read the balance")
		return
	}
	user := identity.User
	if !user.Prepaid {
		respond.JSON(w, creditsBody{IsFreeUser: true})
		return
	}
	respond.JSON(w, creditsBody{
		Balance: float64(user.Budget-user.Spent) / nanoPerUSD,
		Spent:   float64(user.Spent) / nanoPerUSD,
	})
}
