package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func enumerationPeer(t *testing.T, reply func(enumerationRequest, int) (map[string]any, int, error)) *Client {
	t.Helper()
	p := testProfile(t)
	p.Version, p.RequiredProfiles = "0.2", []string{}
	c, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.token, c.catalog = "synthetic-token", maps.Clone(c.local)
	c.selected = &contract{Operations: []string{"resource.enumerate"}}
	c.selected.Enumeration.ResourceTypes = []string{"subject", "source_reference"}
	c.selected.Limits.MaxRequestBytes, c.selected.Limits.MaxResponseBytes = 65536, 1<<20
	c.selected.Limits.MaxEnumerationPageRecords, c.selected.Limits.MaxDependencies, c.selected.Limits.MaxDepth = 32, 16, 16
	n := 0
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/enumerations") || r.Header.Get("SHRIMP-Version") != "0.2" || r.Header.Get("DPoP") == "" || r.Header.Get("Authorization") != "DPoP "+c.token {
			t.Fatal("invalid enumeration request")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		v, err := decodeJSON(raw)
		if err != nil || c.local[enumerationRequest02].Validate(v) != nil {
			t.Fatal("invalid request schema", err)
		}
		var request enumerationRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		value, status, err := reply(request, n)
		if err != nil {
			return nil, err
		}
		if status == 0 {
			status = 200
		}
		if status == 200 {
			scope := value["scope"].(map[string]any)
			if scope["resource"] == "https://example.test" {
				scope["resource"] = p.Resource
			}
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"no-store"}, "Shrimp-Version": []string{"0.2"}}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	return c
}

func enumerationOptions() EnumerationOptions {
	return EnumerationOptions{View: EnumerationView{ResourceTypes: []string{"subject"}, Representation: "full-direct-records"}, PageSize: 1}
}

func enumerationPage(t *testing.T, request enumerationRequest, id string, more bool) map[string]any {
	t.Helper()
	page := contractVector(t, "enumeration-page-more")
	raw, _ := json.Marshal(request.View)
	view, _ := decodeJSON(raw)
	page["view"] = view
	page["records"] = []any{map[string]any{"type": "subject", "id": id, "revision": "r1", "authority": "hr-authority", "deleted": false,
		"value": map[string]any{"profile": "human", "lifecycle": "disabled", "attributes": map[string]any{}, "expires_at": nil}}}
	page["scope"] = map[string]any{"resource": "https://example.test", "tenant": "acme", "domain": "A", "schema_version": "0.2", "history_epoch": "epoch-1", "authorization_context": "context-1"}
	page["more"], page["next_cursor"], page["cursor_expires_at"] = more, nil, nil
	if more {
		page["next_cursor"], page["cursor_expires_at"] = "cursor-"+id, "2026-09-19T20:00:00Z"
	}
	return page
}

func TestEnumerationReplacesRetriedPageAndSuccessor(t *testing.T) {
	var inputs []string
	c := enumerationPeer(t, func(r enumerationRequest, n int) (map[string]any, int, error) {
		cursor := ""
		if r.Cursor != nil {
			cursor = *r.Cursor
		}
		inputs = append(inputs, cursor)
		if len(r.RequiredProfiles) != 0 || r.RequiredProfiles == nil || len(r.Dependencies) != 0 || len(r.View.ResourceTypes) != 1 || r.View.ResourceTypes[0] != "subject" {
			t.Fatal("selection changed")
		}
		ids := []string{"a", "b", "c", "d", "e"}
		return enumerationPage(t, r, ids[n-1], n < 4), 0, nil
	})
	options := enumerationOptions()
	e, err := c.BeginEnumeration(options)
	if err != nil {
		t.Fatal(err)
	}
	options.View.ResourceTypes[0] = "group"
	page, err := e.Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	*page.NextCursor = "caller-tampered"
	page.Records[0].ID = "caller-tampered"
	page.Scope["history_epoch"] = "caller-tampered"
	if _, err := e.Fetch(t.Context()); err == nil {
		t.Fatal("silently restarted at null cursor")
	}
	if err := e.Advance(); err != nil {
		t.Fatal(err)
	}
	// Authentication/discovery no longer describes this live traversal.
	c.selected, c.catalog = nil, nil
	c.token = "renewed-read-only-token"
	if _, err := e.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	page, err = e.Fetch(t.Context())
	if err != nil || page.Records[0].ID != "c" {
		t.Fatal("retry did not replace page", err)
	}
	if err := e.Advance(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.Advance() != io.EOF {
		t.Fatal("terminal page did not end")
	}
	page, err = e.Fetch(t.Context())
	if err != nil || page.Records[0].ID != "e" {
		t.Fatal("final input cursor not retryable", err)
	}
	if !reflect.DeepEqual(inputs, []string{"", "cursor-a", "cursor-a", "cursor-c", "cursor-c"}) {
		t.Fatal("followed abandoned branch", inputs)
	}
}

func TestEnumerationRejectsInvalidPagesAndRetainsInput(t *testing.T) {
	for _, fault := range []string{"scope", "resource", "domain", "epoch", "visibility", "schema", "view", "filter", "record type", "authority", "bounds", "duplicate", "backwards", "overlap", "representation", "empty more", "missing cursor", "terminal cursor", "expiry", "renew expiry", "same cursor", "snapshot field", "transport"} {
		t.Run(fault, func(t *testing.T) {
			c := enumerationPeer(t, func(r enumerationRequest, n int) (map[string]any, int, error) {
				id := "b"
				if n == 1 {
					id = "a"
				}
				page := enumerationPage(t, r, id, true)
				if n != 2 {
					return page, 0, nil
				}
				record := page["records"].([]any)[0].(map[string]any)
				scope := page["scope"].(map[string]any)
				switch fault {
				case "scope":
					scope["tenant"] = "other"
				case "resource":
					scope["resource"] = "https://other.example"
				case "domain":
					scope["domain"] = "other"
				case "epoch":
					scope["history_epoch"] = "other"
				case "visibility":
					scope["authorization_context"] = "other"
				case "schema":
					scope["schema_version"] = "0.1"
				case "view":
					page["view"].(map[string]any)["resource_types"] = []any{"source_reference"}
				case "filter":
					page["view"].(map[string]any)["authority_filter"] = nil
				case "record type":
					record["type"], record["deleted"], record["value"] = "source_reference", true, nil
				case "authority":
					record["authority"] = "other"
				case "bounds":
					page["records"] = []any{record, record, record}
				case "duplicate":
					page["records"] = []any{record, record}
				case "backwards":
					lower := maps.Clone(record)
					lower["id"] = "aa"
					page["records"] = []any{record, lower}
				case "overlap":
					record["id"] = "a"
				case "representation":
					record["value"] = nil
				case "empty more":
					page["records"] = []any{}
				case "missing cursor":
					page["next_cursor"] = nil
				case "terminal cursor":
					page["more"] = false
				case "expiry":
					page["cursor_expires_at"] = "2026-02-31T20:00:00Z"
				case "renew expiry":
					page["cursor_expires_at"] = "2026-09-19T20:00:01Z"
				case "same cursor":
					page["next_cursor"] = *r.Cursor
				case "snapshot field":
					page["complete"] = true
				case "transport":
					return nil, 0, errors.New("lost reply with secret must not escape")
				}
				return page, 0, nil
			})
			options := enumerationOptions()
			options.PageSize, options.View.AuthorityFilter = 2, []string{"hr-authority"}
			e, err := c.BeginEnumeration(options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Fetch(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := e.Advance(); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Fetch(t.Context()); err == nil {
				t.Fatal("fault accepted")
			}
			if err := e.Advance(); err == nil {
				t.Fatal("error advanced a cursor")
			}
			page, err := e.Fetch(t.Context())
			if err != nil || page.Records[0].ID != "b" {
				t.Fatal("valid retry lost original position", err)
			}
		})
	}
}

func TestEnumerationDoesNotAdvanceAnEarlierSuccessAfterRetryFailure(t *testing.T) {
	c := enumerationPeer(t, func(r enumerationRequest, n int) (map[string]any, int, error) {
		if n == 3 {
			return nil, 0, errors.New("lost retry")
		}
		id := "a"
		if n > 1 {
			id = "b"
		}
		return enumerationPage(t, r, id, true), 0, nil
	})
	e, err := c.BeginEnumeration(enumerationOptions())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = e.Advance(); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Fetch(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Fetch(t.Context()); err == nil {
		t.Fatal("missing failure")
	}
	if err = e.Advance(); err == nil {
		t.Fatal("adopted abandoned success")
	}
}

func TestEnumerationBoundsAndSelectionBeforeNetwork(t *testing.T) {
	for _, fault := range []string{"no discovery", "legacy", "no operation", "no schema", "unsupported type", "zero page", "too many records", "duplicate type", "empty filter", "dependency bound", "request bound"} {
		t.Run(fault, func(t *testing.T) {
			c := enumerationPeer(t, func(enumerationRequest, int) (map[string]any, int, error) {
				t.Fatal("sent invalid selection")
				return nil, 0, nil
			})
			opts := enumerationOptions()
			switch fault {
			case "no discovery":
				c.selected = nil
			case "legacy":
				c.profile.Version = "0.1"
			case "no operation":
				c.selected.Operations = nil
			case "no schema":
				delete(c.catalog, enumerationPage02)
			case "unsupported type":
				opts.View.ResourceTypes = []string{"group"}
			case "zero page":
				opts.PageSize = 0
			case "too many records":
				opts.PageSize = 33
			case "duplicate type":
				opts.View.ResourceTypes = []string{"subject", "subject"}
			case "empty filter":
				opts.View.AuthorityFilter = []string{}
			case "dependency bound":
				c.selected.Limits.MaxDependencies = 0
				opts.Dependencies = []string{"opaque"}
			case "request bound":
				c.selected.Limits.MaxRequestBytes = 1
			}
			if _, err := c.BeginEnumeration(opts); err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
	}
}

func TestEnumerationTombstonesUTF8AndSetOrder(t *testing.T) {
	c := enumerationPeer(t, func(r enumerationRequest, _ int) (map[string]any, int, error) {
		page := enumerationPage(t, r, "unused", false)
		page["view"].(map[string]any)["resource_types"] = []any{"source_reference", "subject"}
		var records []any
		for _, ref := range []ResourceRef{{"source_reference", "z"}, {"subject", "Z"}, {"subject", "a"}, {"subject", "é"}} {
			records = append(records, map[string]any{"type": ref.Type, "id": ref.ID, "authority": "hr-authority", "revision": "r1", "deleted": true, "value": nil})
		}
		page["records"] = records
		return page, 0, nil
	})
	opts := enumerationOptions()
	opts.PageSize = 4
	opts.View.ResourceTypes = []string{"subject", "source_reference"}
	e, err := c.BeginEnumeration(opts)
	if err != nil {
		t.Fatal(err)
	}
	page, err := e.Fetch(t.Context())
	if err != nil || len(page.Records) != 4 {
		t.Fatal(err)
	}
}

func TestEnumerationResponseBytes(t *testing.T) {
	for _, limit := range []int{1024, 1 << 20} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			c := enumerationPeer(t, func(r enumerationRequest, _ int) (map[string]any, int, error) {
				page := enumerationPage(t, r, "large", false)
				record := page["records"].([]any)[0].(map[string]any)
				attributes := map[string]any{}
				for _, name := range []string{"displayName", "department", "email"} {
					attributes[name] = map[string]any{"value": strings.Repeat("x", 1024), "authority": strings.Repeat("a", 1024), "revision": strings.Repeat("r", 1024)}
				}
				record["value"].(map[string]any)["attributes"] = attributes
				records := []any{}
				for i := range 32 {
					item := maps.Clone(record)
					item["id"] = fmt.Sprintf("s%02d", i)
					records = append(records, item)
				}
				page["records"] = records
				return page, 0, nil
			})
			c.selected.Limits.MaxResponseBytes = limit
			options := enumerationOptions()
			options.PageSize = 32
			e, err := c.BeginEnumeration(options)
			if err != nil {
				t.Fatal(err)
			}
			_, err = e.Fetch(t.Context())
			if limit == 1024 && !errors.Is(err, errResponseLimit) {
				t.Fatal("response bound ignored", err)
			}
			if limit > 262144 && err != nil {
				t.Fatal("valid page above legacy reader cap rejected", err)
			}
		})
	}
}
