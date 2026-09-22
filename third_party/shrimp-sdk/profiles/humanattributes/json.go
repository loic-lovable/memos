package humanattributes

import (
	"bytes"
	"encoding/json"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/internal/jsontext"
)

// MaxJSONBytes and MaxJSONDepth bound these fragment decoders. The transport must
// separately bound the entire message, including its envelope and other commands.
const (
	MaxJSONBytes = 65536
	MaxJSONDepth = 16
)

func decodeObject(raw []byte) (map[string]any, error) {
	if len(raw) > MaxJSONBytes {
		return nil, failure(ErrLimitExceeded, "", "JSON byte limit exceeded")
	}
	if !jsontext.Valid(raw) {
		return nil, failure(ErrInvalidValue, "", "invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := jsonValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, failure(ErrInvalidValue, "", "expected JSON object")
	}
	return object, nil
}

// json.Valid has established exactly one complete value and correct delimiters.
// Token decoding compares decoded member names, including escaped spellings.
func jsonValue(d *json.Decoder, depth int) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, failure(ErrInvalidValue, "", "invalid JSON")
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	if depth >= MaxJSONDepth {
		return nil, failure(ErrLimitExceeded, "", "JSON depth limit exceeded")
	}
	var result any
	switch delimiter {
	case '{':
		object := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, failure(ErrInvalidValue, "", "invalid JSON")
			}
			name, ok := key.(string)
			if _, duplicate := object[name]; !ok || duplicate {
				return nil, failure(ErrInvalidValue, "", "duplicate JSON member")
			}
			value, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		result = object
	case '[':
		array := []any{}
		for d.More() {
			value, err := jsonValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		result = array
	default:
		return nil, failure(ErrInvalidValue, "", "invalid JSON")
	}
	if _, err := d.Token(); err != nil {
		return nil, failure(ErrInvalidValue, "", "invalid JSON")
	}
	return result, nil
}
