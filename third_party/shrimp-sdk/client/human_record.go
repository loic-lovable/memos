package client

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"
)

// HumanRecord is an observed human subject, with exactly one attribute
// representation. ScalarAttributes is populated only for the compatibility
// representation; HumanAttributes is populated only for human-attributes-v1.
// Missing facts, owned nulls and empty values remain distinct.
type HumanRecord struct {
	ID, Revision, Authority, Lifecycle, AttributeProfile string
	ScalarAttributes                                     map[string]scalar.Fact
	HumanAttributes                                      *humanattributes.Facts
	// Enterprise preserves the complete optional enterprise JSON value without
	// interpreting its context or facts. Its encoding may differ from wire bytes.
	Enterprise json.RawMessage
}

// Version binds a later conditional decision to this exact observed revision.
func (r HumanRecord) Version() SubjectVersion {
	return SubjectVersion{ID: r.ID, Revision: r.Revision, Authority: r.Authority}
}

// ReadHuman returns a typed view of ReadSubject's validated human record. It uses
// the same authenticated read and discovery contract, without applying current
// email limits or timezone catalogs to historical facts. Public facts do not
// contain the private identifier history needed for application transitions.
func (c *Client) ReadHuman(ctx context.Context, id string) (HumanRecord, error) {
	record, err := c.ReadSubject(ctx, id)
	if err != nil {
		return HumanRecord{}, err
	}
	// ReadSubject already checked strict JSON, both schemas, scope and subject
	// identity. Decode that validated value; do not reinterpret it as write input.
	raw, err := json.Marshal(record)
	if err != nil {
		return HumanRecord{}, errors.New("cannot encode validated human record")
	}
	var wire struct {
		ID        string `json:"id"`
		Revision  string `json:"revision"`
		Authority string `json:"authority"`
		Value     struct {
			Lifecycle        string          `json:"lifecycle"`
			AttributeProfile string          `json:"attribute_profile"`
			Attributes       json.RawMessage `json:"attributes"`
			Enterprise       json.RawMessage `json:"enterprise"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return HumanRecord{}, errors.New("cannot decode validated human record")
	}
	result := HumanRecord{ID: wire.ID, Revision: wire.Revision, Authority: wire.Authority,
		Lifecycle: wire.Value.Lifecycle, AttributeProfile: wire.Value.AttributeProfile, Enterprise: wire.Value.Enterprise}
	switch result.AttributeProfile {
	case "":
		if err := json.Unmarshal(wire.Value.Attributes, &result.ScalarAttributes); err != nil {
			return HumanRecord{}, errors.New("invalid scalar human facts")
		}
	case humanattributes.ID:
		result.HumanAttributes = &humanattributes.Facts{}
		if err := json.Unmarshal(wire.Value.Attributes, result.HumanAttributes); err != nil {
			return HumanRecord{}, errors.New("invalid selected human facts")
		}
	default:
		return HumanRecord{}, errors.New("unsupported human representation")
	}
	return result, nil
}
