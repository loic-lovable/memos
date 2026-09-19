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
	for _, shape := range []struct {
		name, prefix, middle, suffix string
	}{
		{"arrays_scalar", "[", "0", "]"},
		{"arrays_empty", "[", "", "]"},
		{"objects_scalar", `{"x":`, "0", "}"},
		{"objects_empty", `{"x":`, "{}", "}"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			for _, depth := range []int{16, 17} {
				count := depth
				if shape.name == "objects_empty" {
					count-- // The innermost {} is also a container.
				}
				input := strings.Repeat(shape.prefix, count) + shape.middle + strings.Repeat(shape.suffix, count)
				var value any
				err := strictJSON([]byte(input), &value)
				if depth == 16 {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, errJSONDepth)
				}
			}
		})
	}
}
