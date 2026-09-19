package shrimp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStrictJSONRejectsAmbiguousIntent(t *testing.T) {
	for _, input := range []string{`{"action":"disable","action":"activate"}`, `{"operation":{"id":"a","id":"b"}}`, `{} {}`, `{"commands":[{"x":1,"x":2}]}`} {
		t.Run(input, func(t *testing.T) { var value map[string]any; require.Error(t, strictJSON([]byte(input), &value)) })
	}
	var value map[string]any
	require.NoError(t, strictJSON([]byte(`{"commands":[{"action":"disable"}]}`), &value))
}
