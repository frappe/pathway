package http

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/phot0n/pathway/internal/domain"
	"github.com/phot0n/pathway/internal/repository"
	"github.com/phot0n/pathway/internal/transport/respond"
)

// The control plane's push/pull surface, token-gated on X-Grove-Admin-Token. Grove is the source of
// truth; these endpoints project its state into this box's store and nothing more.

type adminKey struct {
	KeyHash string `json:"key_hash"` // sha256(secret) hex — the record id
	Prefix  string `json:"prefix"`
	User    string `json:"user"`   // Grove User doc name — the pointer to the user record
	Status  string `json:"status"` // active | revoked
}

type adminUser struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Group   string `json:"group"` // comma list of group names; "" = ungrouped
	Allow   string `json:"allow"` // comma list: this user's adds on top of their groups'
	Deny    string `json:"deny"`  // comma list: removals that beat every grant
	Limited bool   `json:"limited"`
	// LogPayloads opts this user's prompts and outputs into the payload log. Absent on an older
	// control plane's push, which decodes false — off.
	LogPayloads bool   `json:"log_payloads"`
	Geography   string `json:"geography"` // blank = unpinned, as an older push decodes
}

type adminGroup struct {
	Name   string `json:"name"`
	Models string `json:"models"` // comma list; "" = grants nothing
}

// PUT /admin/keys — upsert. DELETE /admin/keys — remove them. Revocation deletes the credential
// rather than flagging it, so the DELETE is what takes a revoked key out of service.
func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.deleteRecords(w, r, s.provisioning.DeleteKeys)
		return
	}
	var body struct {
		Keys []adminKey `json:"keys"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	records := make([]repository.KeyUpsert, 0, len(body.Keys))
	for _, k := range body.Keys {
		records = append(records, repository.KeyUpsert{
			MeterID: k.KeyHash, Prefix: k.Prefix, User: k.User, Status: k.Status,
		})
	}
	if err := s.provisioning.UpsertKeys(r.Context(), records); err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "key store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "count": len(body.Keys)})
}

// PUT /admin/users — upsert the access and budget state behind one or more Grove Users. The point
// of the split: a person's keys are credentials, and this pushes one record instead of one per key.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.deleteRecords(w, r, s.provisioning.DeleteUsers)
		return
	}
	var body struct {
		Users []adminUser `json:"users"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	records := make([]repository.UserUpsert, 0, len(body.Users))
	for _, u := range body.Users {
		records = append(records, repository.UserUpsert{
			Name: u.Name, Email: u.Email, Groups: u.Group,
			Allow: u.Allow, Deny: u.Deny, Limited: u.Limited, LogPayloads: u.LogPayloads,
			Geography: u.Geography,
		})
	}
	if err := s.provisioning.UpsertUsers(r.Context(), records); err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "user store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "count": len(body.Users)})
}

// PUT /admin/groups — upsert what each group grants. Upsert-only:
// a group nobody links to is unreachable, not harmful.
func (s *Server) handleAdminGroups(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Groups []adminGroup `json:"groups"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	records := make([]repository.GroupUpsert, 0, len(body.Groups))
	for _, g := range body.Groups {
		records = append(records, repository.GroupUpsert{
			Name: g.Name, Models: g.Models,
		})
	}
	if err := s.provisioning.UpsertGroups(r.Context(), records); err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "group store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "count": len(body.Groups)})
}

// PUT /admin/routes — replace the routing table for each given model. Grove sends the full healthy
// set per model each sync.
func (s *Server) handleAdminRoutes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Routes map[string][]domain.Route `json:"routes"`
		Prune  bool                      `json:"prune"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	models, pruned, err := s.provisioning.ReplaceRoutes(r.Context(), body.Routes, body.Prune)
	if err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "route store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "models": models, "pruned": pruned})
}

// GET /admin/state-hash — the hashes stored by the last accepted state push, empty on a fresh or
// wiped store. The control plane diffs against this before deciding to push anything at all.
func (s *Server) handleAdminStateHash(w http.ResponseWriter, r *http.Request) {
	hashes, err := s.provisioning.StateHashes(r.Context())
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "state store error")
		return
	}
	if hashes == nil {
		hashes = map[string]string{}
	}
	respond.JSON(w, map[string]any{"hashes": hashes})
}

// POST /admin/state — desired state, whole per section (plan_agent_state_sync.md): apply what is
// named, delete what is not, store the hashes — one transaction. Errors are 500s, never swallowed:
// a push acknowledged but not stored would be divergence no retry ever heals.
func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Groups *struct {
			Hash    string       `json:"hash"`
			Records []adminGroup `json:"records"`
		} `json:"groups"`
		Users *struct {
			Buckets map[string]struct {
				Hash    string      `json:"hash"`
				Records []adminUser `json:"records"`
			} `json:"buckets"`
		} `json:"users"`
		Keys *struct {
			Buckets map[string]struct {
				Hash    string     `json:"hash"`
				Records []adminKey `json:"records"`
			} `json:"buckets"`
		} `json:"keys"`
		Routes *struct {
			Hash  string                    `json:"hash"`
			Table map[string][]domain.Route `json:"table"`
		} `json:"routes"`
	}
	if !decodeBody(w, r, &body) {
		return
	}

	var push repository.StatePush
	if body.Groups != nil {
		records := make([]repository.GroupUpsert, 0, len(body.Groups.Records))
		for _, g := range body.Groups.Records {
			records = append(records, repository.GroupUpsert{Name: g.Name, Models: g.Models})
		}
		push.Groups = &repository.GroupsPush{
			Hash: body.Groups.Hash, Records: records,
		}
	}
	if body.Users != nil {
		push.Users = map[string]repository.UserBucket{}
		for label, bucket := range body.Users.Buckets {
			records := make([]repository.UserUpsert, 0, len(bucket.Records))
			for _, u := range bucket.Records {
				records = append(records, repository.UserUpsert{
					Name: u.Name, Email: u.Email, Groups: u.Group,
					Allow: u.Allow, Deny: u.Deny, Limited: u.Limited, LogPayloads: u.LogPayloads,
					Geography: u.Geography,
				})
			}
			push.Users[label] = repository.UserBucket{Hash: bucket.Hash, Records: records}
		}
	}
	if body.Keys != nil {
		push.Keys = map[string]repository.KeyBucket{}
		for label, bucket := range body.Keys.Buckets {
			records := make([]repository.KeyUpsert, 0, len(bucket.Records))
			for _, k := range bucket.Records {
				records = append(records, repository.KeyUpsert{
					MeterID: k.KeyHash, Prefix: k.Prefix, User: k.User, Status: k.Status,
				})
			}
			push.Keys[label] = repository.KeyBucket{Hash: bucket.Hash, Records: records}
		}
	}
	if body.Routes != nil {
		push.Routes = &repository.RoutesPush{Hash: body.Routes.Hash, Table: body.Routes.Table}
	}

	counts, err := s.provisioning.ApplyState(r.Context(), push)
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "state store error")
		return
	}
	respond.JSON(w, map[string]any{"counts": counts})
}

// GET /admin/usage — pull: atomically read-and-delete every live counter. Mutating, and there is no
// second round trip, so a failed insert control-plane-side drops that cycle's delta rather than
// double-counting it.
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	usages, err := s.provisioning.DrainUsage(r.Context())
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "usage store error")
		return
	}
	respond.JSON(w, map[string]any{"usages": usages})
}

func (s *Server) deleteRecords(w http.ResponseWriter, r *http.Request, remove func(context.Context, []string) (int, error)) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	count, err := remove(r.Context(), body.IDs)
	if err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "count": count})
}

// adminAuth gates every control-plane endpoint on the shared token, compared in constant time. The
// process refuses to start without one, so a blank token here cannot mean "admin off".
func adminAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	want := []byte(token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("X-Grove-Admin-Token"))
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return false
	}
	return true
}
