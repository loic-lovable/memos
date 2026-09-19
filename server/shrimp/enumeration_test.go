package shrimp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestEnumerationSelectionNormalizesSetsButPreservesPresence(t *testing.T) {
	request := func(value string) map[string]any {
		t.Helper()
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(value), &body))
		return body
	}
	first := request(`{"schema_version":"0.2","view":{"resource_types":["subject","source_reference"],"authority_filter":["B","A"],"representation":"full-direct-records"},"page_size":2,"cursor":null,"required_dependencies":["two","one"],"wait_ms":0}`)
	reordered := request(`{"schema_version":"0.2","view":{"resource_types":["source_reference","subject"],"authority_filter":["A","B"],"representation":"full-direct-records"},"page_size":2,"cursor":"next","required_dependencies":["one","two"],"wait_ms":10}`)
	one, err := enumerationSelection(first)
	require.NoError(t, err)
	two, err := enumerationSelection(reordered)
	require.NoError(t, err)
	require.Equal(t, one, two)
	require.Equal(t, []any{"subject", "source_reference"}, first["view"].(map[string]any)["resource_types"], "do not mutate the echoed view")
	reordered["required_profiles"] = []any{}
	explicitEmpty, err := enumerationSelection(reordered)
	require.NoError(t, err)
	require.NotEqual(t, one, explicitEmpty, "omission and [] select distinct traversals")
	delete(reordered, "required_profiles")
	reordered["page_size"] = float64(3)
	changedSize, err := enumerationSelection(reordered)
	require.NoError(t, err)
	require.NotEqual(t, one, changedSize)
}

func TestEnumerationByteLimitMeasuresActualWireIncludingEscaping(t *testing.T) {
	h := &Handler{config: Config{Resource: "https://pilot.example/tenants/acme/domains/A", Authority: "authority<&>"}}
	view := map[string]any{"resource_types": []string{"source_reference", "subject"}, "authority_filter": nil, "representation": "full-direct-records"}
	subject := store.ShrimpSubject{ID: "subject", SourceID: "source", Revision: "r1", SourceRevision: "r0", DisplayName: "<>& é 世界", SourceReference: "external", Lifecycle: "disabled"}
	records := []store.ShrimpRecord{{Type: "source_reference", ID: "source", Subject: subject}, {Type: "subject", ID: "subject", Subject: subject}}
	for _, terminal := range []bool{false, true} {
		page := &store.ShrimpEnumerationPage{Records: records, Frontier: "frontier"}
		if !terminal {
			page.Cursor, page.ExpiresAt = "opaque-cursor", 2000000000
		}
		wire := httptest.NewRecorder()
		send(wire, 200, h.enumerationResponse(view, page))
		for _, margin := range []int{-1, 0} {
			fits := h.enumerationFits(view, wire.Body.Len()+margin)
			// Exercise growing candidates and a later smaller candidate as used
			// when the driver falls back to the last page that fits.
			shorter := *page
			shorter.Records = records[:1]
			_, err := fits(&shorter)
			require.NoError(t, err)
			ok, err := fits(page)
			require.NoError(t, err)
			require.Equal(t, margin == 0, ok, "count serialized bytes, commas, escaping and newline exactly")
			ok, err = fits(&shorter)
			require.NoError(t, err)
			require.True(t, ok)
		}
	}
}
