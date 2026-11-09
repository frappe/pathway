package http

import (
	"math"
	"net/http"
	"strconv"

	"github.com/frappe/pathway/internal/transport/respond"
)

const nanoPerCent = 1e7

// usd is nano-dollars that encode as a JSON number with exactly two decimals, e.g. 12.50.
type usd int64

func (u usd) MarshalJSON() ([]byte, error) {
	return strconv.AppendFloat(nil, math.Round(float64(u)/nanoPerCent)/100, 'f', 2, 64), nil
}

// keyBalanceBody is GET /v1/key/balance, in US dollars to the cent: what this key has left of its
// cap on this store over its window — this month, or for life. The key's, never the team's: the
// path names the calling key, which the bearer identifies. The names are the control plane's own,
// so a holder reads one shape from either side. Balance is what may still be spent, so it floors
// at zero: a key one streamed answer past its cap reads 0.00, and spent says by how much.
type keyBalanceBody struct {
	Balance usd `json:"balance"`
	Spent   usd `json:"spent"`
}

// handleKeyBalance answers what the key has left on this store over its window — the figure the
// quota stage gates on. Its own cap and nothing of the team's, so every key may read it. An
// exhausted key still reads it: that is when it is asked.
func (s *Server) handleKeyBalance(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.identifyCaller(w, r)
	if !ok {
		return
	}
	key := identity.Key
	respond.JSON(w, keyBalanceBody{
		Balance: usd(max(key.Budget-key.Spent, 0)),
		Spent:   usd(key.Spent),
	})
}
