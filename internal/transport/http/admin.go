package http

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/frappe/pathway/internal/service/metering"
	"net/http"
	"strings"
	"time"

	"github.com/frappe/pathway/internal/domain"
	"github.com/frappe/pathway/internal/repository"
	"github.com/frappe/pathway/internal/transport/respond"
)

// The control plane's push/pull surface, token-gated on X-Grove-Admin-Token. Grove is the source of
// truth; these endpoints project its state into this box's store and nothing more.

type adminKey struct {
	KeyHash string `json:"key_hash"` // sha256(secret) hex — the record id
	Prefix  string `json:"prefix"`
	Team    string `json:"team"`   // Grove Team doc name — the tenant boundary
	Status  string `json:"status"` // active | revoked
	Group   string `json:"group"`  // comma list of group names; "" = ungrouped
	Allow   string `json:"allow"`  // comma list: this key's adds on top of its groups'
	Deny    string `json:"deny"`   // comma list: removals that beat every grant
	Limited bool   `json:"limited"`
	// LogPayloads opts this key's prompts and outputs into the payload log. Absent decodes false — off.
	LogPayloads bool   `json:"log_payloads"`
	Geography   string `json:"geography"` // blank = unpinned
	// Budget is the key's cap in nano-USD, what it is gated on. Absent: 0, refused.
	Budget int64 `json:"budget"`
	// BudgetWindow is what the key's spend is counted over for that cap: "month" (or absent: the
	// current UTC month, a new month starts at 0) or "total" (everything since the key was pushed,
	// so a spent key stays spent). Anything else is refused the way a bad limit is.
	BudgetWindow string `json:"budget_window"`
	// Limits is the key's rate limits, a comma list of metric:window:value; "" = none.
	Limits string `json:"limits"`
	// CapMessage is what the 402 says once the key's cap is spent; absent: the default text.
	CapMessage string `json:"cap_message"`
}

// upsert refuses a limit this binary cannot read, the way decodeBody refuses a field it does not
// know: stored, it would be a limit the control plane believes in and nothing enforces.
func (k adminKey) upsert() (repository.KeyUpsert, error) {
	if _, err := domain.ParseLimits(k.Limits); err != nil {
		return repository.KeyUpsert{}, fmt.Errorf("key %s: %w", k.Prefix, err)
	}
	if !domain.IsBudgetWindow(k.BudgetWindow) {
		return repository.KeyUpsert{}, fmt.Errorf("key %s: budget_window %q is not month or total", k.Prefix, k.BudgetWindow)
	}
	return repository.KeyUpsert{
		MeterID: k.KeyHash, Prefix: k.Prefix, Team: k.Team, Status: k.Status, Groups: k.Group,
		Allow: k.Allow, Deny: k.Deny, Limited: k.Limited, LogPayloads: k.LogPayloads,
		Geography: k.Geography, Budget: k.Budget, BudgetWindow: k.BudgetWindow, Limits: k.Limits,
		CapMessage: k.CapMessage,
	}, nil
}

// keyUpserts is every pushed key as the store takes it, or a 400 naming the first one refused.
func keyUpserts(w http.ResponseWriter, keys []adminKey) ([]repository.KeyUpsert, bool) {
	records := make([]repository.KeyUpsert, 0, len(keys))
	for _, k := range keys {
		record, err := k.upsert()
		if err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return nil, false
		}
		records = append(records, record)
	}
	return records, true
}

type adminGroup struct {
	Name   string `json:"name"`
	Models string `json:"models"` // comma list; "" = grants nothing
}

type adminTeam struct {
	Team    string `json:"team"` // Grove Team doc name — the record id
	Limited bool   `json:"limited"`
	// LogPayloads opts the team's keys' prompts and outputs into the payload log. Absent: off.
	LogPayloads bool `json:"log_payloads"`
	// Limits is what a key with none of its own runs under, a comma list of metric:window:value.
	Limits string `json:"limits"`
}

// upsert refuses a limit this binary cannot read, as adminKey.upsert does.
func (t adminTeam) upsert() (repository.TeamUpsert, error) {
	if _, err := domain.ParseLimits(t.Limits); err != nil {
		return repository.TeamUpsert{}, fmt.Errorf("team %s: %w", t.Team, err)
	}
	return repository.TeamUpsert{
		Team: t.Team, Limited: t.Limited, LogPayloads: t.LogPayloads, Limits: t.Limits,
	}, nil
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
	records, ok := keyUpserts(w, body.Keys)
	if !ok {
		return
	}
	if err := s.provisioning.UpsertKeys(r.Context(), records); err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "key store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "count": len(body.Keys)})
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
	if badBody(w, err) {
		return
	}
	if err != nil {
		respond.Error(w, http.StatusServiceUnavailable, "route store error")
		return
	}
	respond.JSON(w, map[string]any{"ok": true, "models": models, "pruned": pruned})
}

// GET /grove-admin/state-hash — the hashes stored by the last accepted state push, empty on a
// fresh or wiped store. The control plane diffs against this before deciding to push anything at
// all. With them, for an operator: `pushed_at`, when a push last landed on this process, and
// `contact_at`, when the control plane last called any admin endpoint — this call included —
// each left out until it has happened.
func (s *Server) handleAdminStateHash(w http.ResponseWriter, r *http.Request) {
	hashes, err := s.provisioning.StateHashes(r.Context())
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "state store error")
		return
	}
	if hashes == nil {
		hashes = map[string]string{}
	}
	reply := map[string]any{"hashes": hashes}
	if at := s.provisioning.PushedAt(); !at.IsZero() {
		reply["pushed_at"] = at.UTC().Format(time.RFC3339)
	}
	if at := s.ContactAt(); !at.IsZero() {
		reply["contact_at"] = at.UTC().Format(time.RFC3339)
	}
	respond.JSON(w, reply)
}

// GET /admin/in-flight — whether new requests are refused, and how many are still running. The
// control plane polls it to know a box in maintenance has gone idle.
func (s *Server) handleAdminInFlight(w http.ResponseWriter, _ *http.Request) {
	respond.JSON(w, map[string]any{"maintenance": s.inMaintenance(), "in_flight": s.inFlight.Load()})
}

func (s *Server) inMaintenance() bool { return s.maintenance != nil && s.maintenance() }

// POST /admin/state — desired state, whole per section (plan_agent_state_sync.md): apply what is
// named, delete what is not, store the hashes — one transaction. Errors are 500s, never swallowed:
// a push acknowledged but not stored would be divergence no retry ever heals.
func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Groups *struct {
			Hash    string       `json:"hash"`
			Records []adminGroup `json:"records"`
		} `json:"groups"`
		Teams *struct {
			Buckets map[string]struct {
				Hash    string      `json:"hash"`
				Records []adminTeam `json:"records"`
			} `json:"buckets"`
		} `json:"teams"`
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
	if body.Teams != nil {
		push.Teams = map[string]repository.TeamBucket{}
		for label, bucket := range body.Teams.Buckets {
			records := make([]repository.TeamUpsert, 0, len(bucket.Records))
			for _, team := range bucket.Records {
				record, err := team.upsert()
				if err != nil {
					http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
					return
				}
				records = append(records, record)
			}
			push.Teams[label] = repository.TeamBucket{Hash: bucket.Hash, Records: records}
		}
	}
	if body.Keys != nil {
		push.Keys = map[string]repository.KeyBucket{}
		for label, bucket := range body.Keys.Buckets {
			records, ok := keyUpserts(w, bucket.Records)
			if !ok {
				return
			}
			push.Keys[label] = repository.KeyBucket{Hash: bucket.Hash, Records: records}
		}
	}
	if body.Routes != nil {
		push.Routes = &repository.RoutesPush{Hash: body.Routes.Hash, Table: body.Routes.Table}
	}

	counts, err := s.provisioning.ApplyState(r.Context(), push)
	if badBody(w, err) {
		return
	}
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "state store error")
		return
	}
	respond.JSON(w, map[string]any{"counts": counts})
}

// GET /grove-admin/provider-keys?ids=a,b — what each vendor credential answered since the last
// call, by the id the control plane pushed it under, and the counts cleared. A key never dialled
// answers zeros.
func (s *Server) handleAdminProviderKeys(w http.ResponseWriter, r *http.Request) {
	ids := commaList(r.URL.Query().Get("ids"))
	if s.providerKeys == nil || len(ids) == 0 {
		respond.JSON(w, map[string]domain.KeyStats{})
		return
	}
	stats, err := s.providerKeys.Take(r.Context(), ids)
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "provider key store error")
		return
	}
	respond.JSON(w, stats)
}

// commaList splits a query value on commas, blanks dropped.
func commaList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// GET /grove-admin/usage[?keys=p1,p2] — pull: live counters set aside under a new drain id (only
// the listed prefixes when keys is given), answered with every counter not yet acknowledged,
// grouped by drain id. Nothing is deleted here, so a key the control plane failed to record is
// answered again on the next pull under its own id, while the rest move on.
func (s *Server) handleAdminUsage(w http.ResponseWriter, r *http.Request) {
	drains, err := s.provisioning.DrainUsage(r.Context(), commaList(r.URL.Query().Get("keys")))
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "usage store error")
		return
	}
	answer := map[string]any{"drains": drains}
	if spool := s.spool(); spool != nil {
		// Lines the store kept refusing, for the control plane to land or record as stuck.
		answer["dead"], answer["spool"] = spool.Dead(), spool.Stats()
	}
	respond.JSON(w, answer)
}

// POST /grove-admin/usage/ack {"acks": {"<drain id>": {"<prefix>": ["<day>", ...]}}, "dead": ["<request id>"]}
// — the control plane has committed these counters, and recorded these dead spool lines. Each is
// kept for the retention window, then expires. A counter not waiting is a no-op, so a retried ack
// is harmless.
func (s *Server) handleAdminUsageAck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Acks repository.Acks `json:"acks"`
		Dead []string        `json:"dead"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	count, err := s.provisioning.AckUsage(r.Context(), body.Acks, s.usageRetention())
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "usage store error")
		return
	}
	if spool := s.spool(); spool != nil && len(body.Dead) > 0 {
		forgot, err := spool.Forget(body.Dead)
		if err != nil {
			respond.Error(w, http.StatusInternalServerError, "usage spool error")
			return
		}
		count += forgot
	}
	respond.JSON(w, map[string]any{"ok": true, "count": count})
}

// GET /grove-admin/spent?ids=a,b — each key's spend this month on this store, nano-USD by the id
// the control plane pushed it under: the figure a cap check is made on, off the counter the cap
// is gated on. A key not held here is left out; the drains are not touched.
func (s *Server) handleAdminSpent(w http.ResponseWriter, r *http.Request) {
	spent, err := s.provisioning.Spent(r.Context(), commaList(r.URL.Query().Get("ids")))
	if err != nil {
		respond.Error(w, http.StatusInternalServerError, "key store error")
		return
	}
	respond.JSON(w, spent)
}

// POST /grove-admin/spend-adjust {"key", "delta", "id"} — correct one key's spend for the
// current month on this store by delta nano-USD, once per id: a retry answers applied=false and
// moves nothing.
// key is the record id, sha256(secret) hex.
func (s *Server) handleAdminSpendAdjust(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key   string `json:"key"`
		Delta int64  `json:"delta"`
		ID    string `json:"id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Key == "" || body.ID == "" {
		http.Error(w, "key and id are required", http.StatusBadRequest)
		return
	}
	spent, applied, found, err := s.provisioning.AdjustSpent(r.Context(), body.Key, body.ID, body.Delta)
	switch {
	case err != nil:
		respond.Error(w, http.StatusInternalServerError, "key store error")
	case !found:
		respond.Error(w, http.StatusNotFound, "no such key on this store")
	default:
		respond.JSON(w, map[string]any{"spent": spent, "applied": applied})
	}
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

// decodeBody refuses a field this binary does not know. A push the box cannot keep whole is an
// error the control plane must see — a row stored without the field, under the hash of the full
// payload, is drift no later push would notice.
func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// spool is the usage spool, or nil on a box that keeps none.
func (s *Server) spool() *metering.Spool {
	if s.metering == nil {
		return nil
	}
	return s.metering.Spool
}

// badBody answers a push provisioning refused for what it said, not for where it was going: a
// domain.Denial is the payload's fault, as `bad body` is everywhere else here. Reports whether it
// answered; any other error, or none, is the caller's.
func badBody(w http.ResponseWriter, err error) bool {
	var denial domain.Denial
	if !errors.As(err, &denial) {
		return false
	}
	http.Error(w, "bad body: "+denial.Reason, denial.Status)
	return true
}
