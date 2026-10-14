package http

import (
	"math"
	"net/http"
	"strconv"

	"github.com/phot0n/pathway/internal/transport/respond"
)

const nanoPerCent = 1e7

// usd is nano-dollars that encode as a JSON number with exactly two decimals, e.g. 12.50.
type usd int64

func (u usd) MarshalJSON() ([]byte, error) {
	v := math.Round(float64(u)/nanoPerCent) / 100
	if v == 0 {
		v = 0 // never "-0.00"
	}
	return strconv.AppendFloat(nil, v, 'f', 2, 64), nil
}

// creditsBody is GET /v1/credits, in US dollars to the cent: what this key has left of its
// monthly cap on this store, this month. The names are the control plane's own, so a holder reads one shape
// from either side.
type creditsBody struct {
	Balance usd `json:"balance"`
	Spent   usd `json:"spent"`
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
		Balance: usd(key.Budget - key.Spent),
		Spent:   usd(key.Spent),
	})
}
