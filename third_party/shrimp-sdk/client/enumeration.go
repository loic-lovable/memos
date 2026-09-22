package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const enumerationRequest02 = "read-sync-v0.2.schema.json#/$defs/enumeration_request"
const enumerationPage02 = "read-sync-v0.2.schema.json#/$defs/enumeration_page"
const enumerationClientBytes = 4 << 20

// ResourceRef identifies a direct record without including its private contents.
type ResourceRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// EnumerationView selects full records within the enrolled authority. A nil
// AuthorityFilter selects current authorized visibility, not a global directory.
type EnumerationView struct {
	ResourceTypes   []string `json:"resource_types"`
	AuthorityFilter []string `json:"authority_filter"`
	Representation  string   `json:"representation"`
}

type EnumerationOptions struct {
	View         EnumerationView `json:"view"`
	PageSize     int             `json:"page_size"`
	Dependencies []string        `json:"required_dependencies"`
}

type enumerationRequest struct {
	SchemaVersion string `json:"schema_version"`
	EnumerationOptions
	Cursor           *string  `json:"cursor"`
	WaitMS           int      `json:"wait_ms"`
	RequiredProfiles []string `json:"required_profiles"`
}

// EnumerationRecord retains the full validated representation, including tombstones.
type EnumerationRecord struct {
	ResourceRef
	Revision  string          `json:"revision"`
	Authority string          `json:"authority"`
	Deleted   bool            `json:"deleted"`
	Value     json.RawMessage `json:"value"`
}

type EnumerationPage struct {
	Scope            map[string]string   `json:"scope"`
	View             EnumerationView     `json:"view"`
	ObservedFrontier string              `json:"observed_frontier"`
	Consistency      string              `json:"consistency"`
	Ordering         string              `json:"ordering"`
	Records          []EnumerationRecord `json:"records"`
	More             bool                `json:"more"`
	NextCursor       *string             `json:"next_cursor"`
	CursorExpiresAt  *string             `json:"cursor_expires_at"`
}

// Enumeration is a bounded, in-memory page consumer for portable tests. Fetch
// retries the same input cursor and replaces its page; Advance adopts only the
// latest successful page. It never accumulates a reconciliation inventory.
// Calls are serialized to prevent late responses from reviving an old branch.
// Use the client sequentially: authentication must not run concurrently with Fetch.
type Enumeration struct {
	mu                                sync.Mutex
	client                            *Client
	request                           enumerationRequest
	requestSchema, pageSchema         *jsonschema.Schema
	maxRequestBytes, maxResponseBytes int
	scope                             map[string]string
	expires                           string
	after                             *ResourceRef
	page                              *EnumerationPage
}

func (c *Client) BeginEnumeration(options EnumerationOptions) (*Enumeration, error) {
	if c.profile.Version != "0.2" || c.selected == nil || !slices.Contains(c.selected.Operations, "resource.enumerate") {
		return nil, unavailable("0.2 enumeration discovery is unavailable")
	}
	if c.catalog[enumerationRequest02] == nil || c.catalog[enumerationPage02] == nil {
		return nil, unavailable("enumeration schema definitions are not advertised")
	}
	options.View.ResourceTypes = slices.Clone(options.View.ResourceTypes)
	options.View.AuthorityFilter = slices.Clone(options.View.AuthorityFilter)
	options.Dependencies = append([]string{}, options.Dependencies...)
	for _, kind := range options.View.ResourceTypes {
		if !slices.Contains(c.selected.Enumeration.ResourceTypes, kind) {
			return nil, unavailable("selected enumeration resource type is not advertised")
		}
	}
	if options.PageSize > c.selected.Limits.MaxEnumerationPageRecords || len(options.Dependencies) > c.selected.Limits.MaxDependencies || c.selected.Limits.MaxDepth < 3 || c.selected.Limits.MaxResponseBytes < 1 {
		return nil, unavailable("enumeration selection exceeds advertised limits")
	}
	e := &Enumeration{client: c, request: enumerationRequest{SchemaVersion: "0.2", EnumerationOptions: options, RequiredProfiles: c.profile.DiscoveryProfiles()},
		requestSchema: c.catalog[enumerationRequest02], pageSchema: c.catalog[enumerationPage02],
		maxRequestBytes: c.selected.Limits.MaxRequestBytes, maxResponseBytes: c.selected.Limits.MaxResponseBytes}
	if _, err := e.body(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Enumeration) body() ([]byte, error) {
	raw, err := json.Marshal(e.request)
	if err != nil {
		return nil, err
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(raw) > e.maxRequestBytes || e.client.local[enumerationRequest02].Validate(value) != nil || e.requestSchema.Validate(value) != nil {
		return nil, unavailable("enumeration request exceeds the selected contract")
	}
	return raw, nil
}

// Fetch retains its original selection and bounds across credential renewal or
// discovery withdrawal. Any error leaves the same input cursor available, but
// prevents Advance from adopting a previously accepted response to that cursor.
func (e *Enumeration) Fetch(ctx context.Context) (EnumerationPage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.request.Cursor == nil && e.page != nil {
		return EnumerationPage{}, errors.New("advance the first page before fetching again; cursor null would start another traversal")
	}
	e.page = nil
	raw, err := e.body()
	if err != nil {
		return EnumerationPage{}, err
	}
	r, err := e.client.apiWithLimit(ctx, "POST", "/enumerations", "0.2", string(raw), int64(min(e.maxResponseBytes, enumerationClientBytes)))
	if errors.Is(err, errResponseLimit) && e.maxResponseBytes > enumerationClientBytes {
		return EnumerationPage{}, unavailable("enumeration page exceeds this client's 4 MiB observation limit")
	}
	if err != nil {
		return EnumerationPage{}, err
	}
	if r.status != 200 {
		return EnumerationPage{}, unavailable("enumeration observation unavailable (HTTP %d); original cursor retained, no automatic restart", r.status)
	}
	if !media(r.header, "application/json") || e.client.local[enumerationPage02].Validate(r.value) != nil || e.pageSchema.Validate(r.value) != nil {
		return EnumerationPage{}, errors.New("enumeration page violates its wire schema")
	}
	var page EnumerationPage
	if err := json.Unmarshal(r.raw, &page); err != nil {
		return EnumerationPage{}, errors.New("invalid enumeration representation")
	}
	if !e.client.scope02(r.value) || (e.scope != nil && !maps.Equal(e.scope, page.Scope)) {
		return EnumerationPage{}, errors.New("enumeration scope or visibility changed")
	}
	if !sameView(e.request.View, page.View) || len(page.Records) > e.request.PageSize {
		return EnumerationPage{}, errors.New("enumeration view or page bound changed")
	}
	previous := e.after
	for _, record := range page.Records {
		if previous != nil && !resourceLess(*previous, record.ResourceRef) {
			return EnumerationPage{}, errors.New("enumeration repeated or misordered a resource")
		}
		if !slices.Contains(e.request.View.ResourceTypes, record.Type) || (e.request.View.AuthorityFilter != nil && !slices.Contains(e.request.View.AuthorityFilter, record.Authority)) {
			return EnumerationPage{}, errors.New("enumeration returned a resource outside the selected view")
		}
		current := record.ResourceRef
		previous = &current
	}
	if page.More {
		if e.request.Cursor != nil && *page.NextCursor == *e.request.Cursor {
			return EnumerationPage{}, errors.New("enumeration cursor made no progress")
		}
		if _, err := time.Parse(time.RFC3339, *page.CursorExpiresAt); err != nil {
			return EnumerationPage{}, errors.New("invalid enumeration cursor expiry")
		}
		if e.expires != "" && *page.CursorExpiresAt != e.expires {
			return EnumerationPage{}, errors.New("enumeration changed the fixed cursor expiry")
		}
		e.expires = *page.CursorExpiresAt
	}
	e.scope = maps.Clone(page.Scope)
	e.page = &page
	// Return a separate representation so callers cannot alter the accepted
	// continuation, order position, records, or pinned scope through aliases.
	var result EnumerationPage
	if err := json.Unmarshal(r.raw, &result); err != nil {
		return EnumerationPage{}, err
	}
	return result, nil
}

// Advance follows only the latest successfully fetched page. A final page stays
// retryable through its original input cursor even after Advance returns EOF.
func (e *Enumeration) Advance() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.page == nil {
		return errors.New("fetch a valid enumeration page before advancing")
	}
	if !e.page.More {
		return io.EOF
	}
	e.request.Cursor = e.page.NextCursor
	last := e.page.Records[len(e.page.Records)-1].ResourceRef
	e.after = &last
	e.page = nil
	return nil
}

func resourceLess(a, b ResourceRef) bool { return a.Type < b.Type || a.Type == b.Type && a.ID < b.ID }

func sameView(a, b EnumerationView) bool {
	equalSet := func(x, y []string) bool {
		x, y = slices.Clone(x), slices.Clone(y)
		slices.Sort(x)
		slices.Sort(y)
		return slices.Equal(x, y)
	}
	return a.Representation == b.Representation && (a.AuthorityFilter == nil) == (b.AuthorityFilter == nil) && equalSet(a.ResourceTypes, b.ResourceTypes) && equalSet(a.AuthorityFilter, b.AuthorityFilter)
}
