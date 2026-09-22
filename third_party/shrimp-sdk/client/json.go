package client

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/internal/jsontext"
)

// ParseDocument uses the same bounded, duplicate-free JSON parser as the wire
// client for portable test manifests. It does not fetch references or run code.
func ParseDocument(raw []byte) (any, error) { return decodeJSON(raw) }

// decodeJSON rejects ambiguous JSON instead of accepting encoding/json's last
// duplicate member or replacement of invalid Unicode. Limits apply before parsing.
func decodeJSON(raw []byte) (any, error) {
	return decodeJSONLimit(raw, 262144)
}

func decodeJSONLimit(raw []byte, maximum int64) (any, error) {
	if int64(len(raw)) > maximum || !jsontext.Valid(raw) {
		return nil, errors.New("invalid or oversized JSON response")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return jsonValue(d, 0)
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth > 32 {
		return nil, errors.New("JSON nesting exceeds 32 levels")
	}
	t, err := d.Token()
	if err != nil {
		return nil, errors.New("invalid JSON token")
	}
	switch t {
	case json.Delim('{'):
		m := map[string]any{}
		for d.More() {
			key, _ := d.Token() // json.Valid already established syntax and key types.
			name := key.(string)
			if _, exists := m[name]; exists {
				return nil, errors.New("duplicate JSON member")
			}
			value, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			m[name] = value
		}
		_, err = d.Token()
		return m, err
	case json.Delim('['):
		items := []any{}
		for d.More() {
			value, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			items = append(items, value)
		}
		_, err = d.Token()
		return items, err
	default:
		return t, nil
	}
}
