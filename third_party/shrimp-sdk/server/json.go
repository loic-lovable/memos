package server

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/pkg/errors"
)

var errJSONDepth = errors.New("JSON nesting limit")

// strictJSON rejects ambiguous duplicate members before typed decoding, including
// nested members. A duplicate cannot change authority or logical retry equality.
func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delimiter, ok := token.(json.Delim); ok {
			// Count containers from one; empty containers consume depth too.
			if depth >= 16 {
				return errJSONDepth
			}
			switch delimiter {
			case '{':
				seen := map[string]bool{}
				for decoder.More() {
					key, err := decoder.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || seen[name] {
						return errors.New("duplicate JSON member")
					}
					seen[name] = true
					if err := value(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for decoder.More() {
					if err := value(depth + 1); err != nil {
						return err
					}
				}
			default:
				return errors.New("invalid JSON delimiter")
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
