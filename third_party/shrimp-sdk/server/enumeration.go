package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"

	"github.com/pkg/errors"
)

func (h *Handler) directRecord(record Record) map[string]any {
	s := record.Subject
	revision := s.Revision
	var value any = map[string]any{"profile": "human", "lifecycle": s.Lifecycle, "expires_at": nil, "attributes": map[string]any{"displayName": map[string]any{"value": s.DisplayName, "authority": h.config.Authority, "revision": s.Revision}}}
	if record.Type == "source_reference" {
		revision = s.SourceRevision
		value = map[string]any{"subject": ref("subject", s.ID), "external_id": s.SourceReference, "state": "associated"}
	}
	return map[string]any{"type": record.Type, "id": record.ID, "revision": revision, "authority": h.config.Authority, "deleted": false, "value": value}
}

func enumerationSelection(body map[string]any) (string, error) {
	// Work on a copy so the response can echo the client's exact selected view.
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	var selection map[string]any
	if err := json.Unmarshal(encoded, &selection); err != nil {
		return "", err
	}
	delete(selection, "cursor")
	delete(selection, "wait_ms")
	view := selection["view"].(map[string]any)
	for _, pair := range []struct {
		object map[string]any
		name   string
	}{{selection, "required_profiles"}, {selection, "required_dependencies"}, {view, "resource_types"}, {view, "authority_filter"}} {
		if values, ok := pair.object[pair.name].([]any); ok {
			slices.SortFunc(values, func(a, b any) int {
				if a.(string) < b.(string) {
					return -1
				}
				if a.(string) > b.(string) {
					return 1
				}
				return 0
			})
		}
	}
	encoded, err = json.Marshal(selection)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

func (h *Handler) enumerationResponse(view any, page *EnumerationPage) map[string]any {
	records := make([]any, 0, len(page.Records))
	for _, record := range page.Records {
		records = append(records, h.directRecord(record))
	}
	var cursor, expiry any
	if page.Cursor != "" {
		cursor, expiry = page.Cursor, stamp(page.ExpiresAt)
	}
	return map[string]any{"scope": h.scope(), "view": view, "observed_frontier": page.Frontier,
		"consistency": "per-page-current", "ordering": "type-id-utf8-ascending", "records": records,
		"more": page.Cursor != "", "next_cursor": cursor, "cursor_expires_at": expiry}
}

func (h *Handler) enumerationFits(view any, limit int) func(*EnumerationPage) (bool, error) {
	// Candidates are prefixes of one immutable observation. Measure each record
	// once; large attributes must not make page preparation quadratic in bytes.
	prefixBytes := []int{0}
	return func(page *EnumerationPage) (bool, error) {
		for len(prefixBytes) <= len(page.Records) {
			index := len(prefixBytes) - 1
			encoded, err := json.Marshal(h.directRecord(page.Records[index]))
			if err != nil {
				return false, err
			}
			prefixBytes = append(prefixBytes, prefixBytes[index]+len(encoded))
		}
		envelope := *page
		envelope.Records = nil
		encoded, err := json.Marshal(h.enumerationResponse(view, &envelope))
		n := len(page.Records)
		return len(encoded)+prefixBytes[n]+max(0, n-1)+1 <= limit, err // commas and send's newline.
	}
}

func (h *Handler) enumerate(w http.ResponseWriter, r *http.Request, claims *accessClaims) {
	body, err := h.body(r, "enumeration")
	if err != nil {
		h.bodyProblem(w, err, "read")
		return
	}
	if profiles, ok := body["required_profiles"].([]any); ok && len(profiles) > 0 {
		h.problem(w, 400, "unsupported_profile", "read")
		return
	}
	if body["wait_ms"] != float64(0) {
		h.problem(w, 400, "limit_exceeded", "read")
		return
	}
	selection, err := enumerationSelection(body)
	if err != nil {
		h.problem(w, 503, "unavailable", "read")
		return
	}
	view := body["view"].(map[string]any)
	scope, err := json.Marshal([]string{h.config.Resource, h.config.Issuer, h.config.ClientID})
	if err != nil {
		h.problem(w, 503, "unavailable", "read")
		return
	}
	request := Enumeration{Principal: claims.Subject, Scope: string(scope), Authorization: h.config.Authority,
		Epoch: h.scope()["history_epoch"].(string), Selection: selection, PageSize: int(body["page_size"].(float64)), Visible: true}
	request.Cursor, _ = body["cursor"].(string)
	for _, v := range view["resource_types"].([]any) {
		request.Types = append(request.Types, v.(string))
	}
	for _, v := range body["required_dependencies"].([]any) {
		request.Dependencies = append(request.Dependencies, v.(string))
	}
	if filter, ok := view["authority_filter"].([]any); ok {
		request.Visible = slices.Contains(filter, any(h.config.Authority))
	}
	page, err := h.driver.Enumerate(r.Context(), request, h.enumerationFits(view, 1048576))
	if err != nil {
		status, code := 503, "unavailable"
		var failure EnumerationError
		if errors.As(err, &failure) {
			if selected, ok := map[string]int{"invalid_cursor": 400, "unsupported_resource": 400, "limit_exceeded": 400,
				"invalid_dependency": 409, "cursor_scope_mismatch": 403, "view_changed": 409, "cursor_epoch_mismatch": 409,
				"cursor_expired": 410, "consistency_unavailable": 503, "throttled": 429}[string(failure)]; ok {
				status, code = selected, string(failure)
			}
		}
		if status == 429 {
			h.throttled(w, "read")
			return
		}
		h.problem(w, status, code, "read")
		return
	}
	response := h.enumerationResponse(view, page)
	if err := h.resolved["enumeration_page"].Validate(response); err != nil {
		h.problem(w, 500, "invalid_enumeration_page", "read")
		return
	}
	send(w, 200, response)
}
