package shrimp

import (
	"strings"
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

func TestStrictJSONDepthBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
		want  error
	}{
		{"exact", 16, nil},
		{"above", 17, errJSONDepth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Repeat("[", tc.depth) + "0" + strings.Repeat("]", tc.depth)
			var value any
			err := strictJSON([]byte(input), &value)
			if tc.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
}
