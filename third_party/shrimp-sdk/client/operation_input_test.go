package client

import (
	"net/http"
	"strings"
	"testing"
)

func TestScalarOperationInvalidInputFailsBeforeNetwork(t *testing.T) {
	invalidUTF8 := string([]byte{'a', 0xff, 'b'})
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *Client) (Intent, error)
	}{
		{"create value", func(t *testing.T, c *Client) (Intent, error) {
			return c.PrepareCreate(t.Context(), Human{Authority: "hr-authority", SourceReference: "employee-1", DisplayName: invalidUTF8})
		}},
		{"update value", func(t *testing.T, c *Client) (Intent, error) {
			return c.PrepareUpdate(t.Context(), humanSubjectVersion(), ScalarChanges{Set: map[string]string{"email": invalidUTF8}})
		}},
		{"compound value", func(t *testing.T, c *Client) (Intent, error) {
			return c.PrepareUpdateLifecycle(t.Context(), humanSubjectVersion(), ScalarChanges{Set: map[string]string{"department": invalidUTF8}}, "disabled")
		}},
		{"oversized value", func(t *testing.T, c *Client) (Intent, error) {
			return c.PrepareCreate(t.Context(), Human{Authority: "hr-authority", SourceReference: "employee-1", DisplayName: strings.Repeat("界", 1025)})
		}},
		{"source reference", func(t *testing.T, c *Client) (Intent, error) {
			return c.PrepareCreate(t.Context(), Human{Authority: "hr-authority", SourceReference: invalidUTF8, DisplayName: "Maya"})
		}},
		{"authority", func(t *testing.T, c *Client) (Intent, error) {
			s := humanSubjectVersion()
			s.Authority = invalidUTF8
			return c.PrepareUpdate(t.Context(), s, ScalarChanges{Clear: []string{"department"}})
		}},
		{"subject", func(t *testing.T, c *Client) (Intent, error) {
			s := humanSubjectVersion()
			s.ID = invalidUTF8
			return c.PrepareDisable(t.Context(), s)
		}},
		{"revision", func(t *testing.T, c *Client) (Intent, error) {
			s := humanSubjectVersion()
			s.Revision = strings.Repeat("r", 1025)
			return c.PrepareUpdateLifecycle(t.Context(), s, ScalarChanges{Clear: []string{"email"}}, "retired")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := operationClient(t)
			c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid local input contacted the peer")
				return nil, nil
			})
			intent, err := tc.prepare(t, c)
			if err == nil || intent != (Intent{}) {
				t.Fatalf("invalid input produced intent: %+v, error: %v", intent, err)
			}
		})
	}
}
