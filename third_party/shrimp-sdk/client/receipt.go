package client

import (
	"encoding/json"
	"errors"
	"math/big"
	"time"
)

// validateReceipt02 follows embedded schema validation and checks the bindings
// and time relationships it cannot express. No current discovery, history epoch or authorization revision
// can replace the original receipt's scope or shorten its recorded promises.
func (c *Client) validateReceipt02(receipt map[string]any) error {
	scope := receipt["scope"].(map[string]any)
	if scope["resource"] != c.profile.Resource || scope["tenant"] != c.profile.Tenant ||
		scope["domain"] != c.profile.Domain || scope["schema_version"] != c.profile.Version {
		return errors.New("receipt belongs to a different resource, tenant/domain or version")
	}
	accepted, err := time.Parse(time.RFC3339, receipt["accepted_at"].(string))
	if err != nil {
		return errors.New("receipt has an invalid acceptance time")
	}
	last := accepted
	if terminal, ok := receipt["terminal_at"].(string); ok {
		last, err = time.Parse(time.RFC3339, terminal)
		if err != nil || last.Before(accepted) {
			return errors.New("receipt has an invalid terminal time")
		}
	}
	retained, err := time.Parse(time.RFC3339, receipt["results_retained_until"].(string))
	minimum, valid := new(big.Rat).SetString(receipt["min_result_retention_seconds"].(json.Number).String())
	// The original replay-window close may set a later floor. A status-only
	// reader has no window evidence, so it can check only this lower bound.
	if err != nil || !valid || minimum.Cmp(new(big.Rat).SetInt64(retained.Unix()-last.Unix())) > 0 {
		return errors.New("receipt contradicts its minimum result retention")
	}
	for _, item := range receipt["effects"].([]any) {
		effect := item.(map[string]any)
		deadline, err := time.Parse(time.RFC3339, effect["deadline"].(string))
		if err != nil || deadline.Before(accepted) {
			return errors.New("receipt has an invalid effect deadline")
		}
	}
	return nil
}
