package humanattributes

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodersRejectAmbiguousJSONAndInvalidUnicode(t *testing.T) {
	p := profile(t)
	for _, tc := range []struct{ name, raw string }{
		{"empty document", ""}, {"trailing document", "{} {}"}, {"trailing garbage", "{}x"},
		{"duplicate attribute", `{"displayName":"private-a","displayName":"private-b"}`},
		{"escaped duplicate", `{"displayName":"private-a","display\u004eame":"private-b"}`},
		{"nested duplicate", `{"name":{"givenName":"private-a","givenName":"private-b"}}`},
		{"email duplicate", `{"emails":[{"entry_id":"work","value":"a@b","primary":false,"primary":true,"expected_generation":null}]}`},
		{"unknown nested duplicate", `{"unknown":{"x":null,"x":false}}`},
		{"duplicate set", `{"set":{},"set":{"department":"new"},"clear":[]}`},
		{"low surrogate", `{"displayName":"\udc00"}`}, {"high surrogate", `{"displayName":"\ud800"}`},
		{"broken pair", `{"displayName":"\ud800\u0041"}`}, {"reversed pair", `{"displayName":"\udc00\ud800"}`},
		{"unpaired member", `{"\ud800":"x"}`}, {"invalid UTF8", "{\"displayName\":\"\xff\"}"},
		{"scalar", `true`}, {"null", `null`}, {"array", `[]`}, {"truncated", `{"displayName":"\u`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := p.DecodeValues([]byte(tc.raw))
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Values{}, values)
			require.NotContains(t, err.Error(), "private")
			changes, err := p.DecodeChanges([]byte(tc.raw))
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Changes{}, changes)
		})
	}
	for _, tc := range []struct{ name, raw, value string }{
		{"pair", `{"displayName":"\ud83d\ude00"}`, "😀"},
		{"escaped escape", `{"displayName":"\\ud800"}`, `\ud800`},
		{"literal replacement", `{"displayName":"�"}`, "�"},
		{"escaped replacement", `{"displayName":"\ufffd"}`, "�"},
		{"escaped key", `{"display\u004eame":"Maya"}`, "Maya"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := p.DecodeValues([]byte(tc.raw))
			require.NoError(t, err)
			require.Equal(t, tc.value, *values.DisplayName)
		})
	}
}

func TestJSONByteAndContainerDepthBoundaries(t *testing.T) {
	p := profile(t)
	for _, size := range []int{MaxJSONBytes - 1, MaxJSONBytes, MaxJSONBytes + 1} {
		t.Run(strconv.Itoa(size)+" bytes", func(t *testing.T) {
			raw := []byte(`{}` + strings.Repeat(" ", size-2))
			values, err := p.DecodeValues(raw)
			if size > MaxJSONBytes {
				require.ErrorIs(t, err, ErrLimitExceeded)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, Values{}, values)
		})
	}
	const change = `{"set":{},"clear":["department"]}`
	_, err := p.DecodeChanges([]byte(change + strings.Repeat(" ", MaxJSONBytes-len(change))))
	require.NoError(t, err)
	changes, err := p.DecodeChanges([]byte(change + strings.Repeat(" ", MaxJSONBytes+1-len(change))))
	require.ErrorIs(t, err, ErrLimitExceeded)
	require.Equal(t, Changes{}, changes)
	for _, leaf := range []string{"0", "[]", "{}"} {
		t.Run("depth leaf "+leaf, func(t *testing.T) {
			containers := MaxJSONDepth - 1 // Plus the root object.
			if leaf != "0" {
				containers-- // Empty containers also consume one level.
			}
			atLimit := `{"extra":` + strings.Repeat("[", containers) + leaf + strings.Repeat("]", containers) + `}`
			values, err := p.DecodeValues([]byte(atLimit))
			require.ErrorIs(t, err, ErrInvalidValue, "depth is allowed but the attribute is unknown")
			require.NotErrorIs(t, err, ErrLimitExceeded)
			require.Equal(t, Values{}, values)
			tooDeep := `{"extra":` + strings.Repeat("[", containers+1) + leaf + strings.Repeat("]", containers+1) + `}`
			values, err = p.DecodeValues([]byte(tooDeep))
			require.ErrorIs(t, err, ErrLimitExceeded)
			require.Equal(t, Values{}, values)
		})
	}
}
