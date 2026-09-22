package client

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client/internal/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func probePeer(t *testing.T, fault string) *Client {
	t.Helper()
	c, err := New(testProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.token = "test-token"
	c.catalog = map[string]*jsonschema.Schema{"operation-receipt.schema.json": c.local["operation-receipt.schema.json"]}
	c.versions = map[string]bool{"0.1": true}
	schemaBytes, _ := schemas.Files.ReadFile("operation-receipt.schema.json")
	c.schemaDigests = map[string]string{"operation-receipt.schema.json": fmt.Sprintf("%x", sha256.Sum256(schemaBytes))}
	discovery := discoveryFixture(t)
	discovery["resource"] = c.profile.Resource
	seen := map[string]bool{}
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Error("probe attempted a mutation")
		}
		status, code := 200, ""
		proof := r.Header.Get("DPoP")
		switch {
		case fault == "deny all":
			status, code = 403, "forbidden"
		case r.Header.Get("Authorization") == "" && fault != "public discovery":
			status, code = 401, "unauthenticated"
		case seen[proof] && fault == "nonce changed":
			status, code = 401, "use_dpop_nonce"
		case seen[proof] && fault != "accept replay":
			status, code = 401, "invalid_dpop_proof"
		case strings.Contains(r.URL.Path, "/schemas/") && r.Header.Get("SHRIMP-Version") == "" && fault != "ignore version":
			status, code = 400, "version_required"
		case strings.Contains(r.URL.Path, "/schemas/") && r.Header.Get("SHRIMP-Version") != "0.1" && fault != "ignore version":
			status, code = 400, "unsupported_version"
		}
		seen[proof] = true
		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		headers.Set("Cache-Control", "no-store")
		var value any = discovery
		if status >= 400 {
			headers.Set("Content-Type", "application/problem+json")
			if status == 401 && fault != "omit challenge" {
				headers.Set("WWW-Authenticate", `DPoP error="`+code+`"`)
			}
			value = map[string]any{"type": "about:blank", "title": "Rejected", "status": status, "code": code, "recovery": map[string]any{"action": "repair"}}
			if fault == "invalid problem" {
				value.(map[string]any)["status"] = 200
			}
		} else if strings.Contains(r.URL.Path, "/schemas/") {
			headers.Set("SHRIMP-Version", "0.1")
			headers.Set("Content-Type", "application/schema+json")
			value, _ = decodeJSON(schemaBytes)
		}
		raw, _ := json.Marshal(value)
		if status == 200 && strings.Contains(r.URL.Path, "/schemas/") {
			raw = schemaBytes
		}
		if status == 200 && fault == "invalid control" {
			raw = []byte(`{"unrelated":true}`)
		}
		if status == 200 && fault == "schema changed" {
			raw = append(append([]byte{}, raw...), '\n')
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
	})
	return c
}

func TestProbeControlsAndFaultDetection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe Probe
		fault string
		want  string
	}{
		{"reject no credentials", Unauthenticated, "", "pass"},
		{"reject missing version", MissingVersion, "", "pass"},
		{"reject unknown version", UnknownVersion, "", "pass"},
		{"reject accepted proof", ReplayedProof, "", "pass"},
		{"catch public discovery", Unauthenticated, "public discovery", "fail"},
		{"catch missing version accepted", MissingVersion, "ignore version", "fail"},
		{"catch unknown version accepted", UnknownVersion, "ignore version", "fail"},
		{"catch replay accepted", ReplayedProof, "accept replay", "fail"},
		{"catch missing DPoP challenge", ReplayedProof, "omit challenge", "fail"},
		{"catch malformed problem", MissingVersion, "invalid problem", "fail"},
		{"always deny is not a pass", ReplayedProof, "deny all", "inconclusive"},
		{"nonce change is not replay evidence", ReplayedProof, "nonce changed", "inconclusive"},
		{"HTTP 200 alone is not a control", Unauthenticated, "invalid control", "fail"},
		{"schema control must match catalog", MissingVersion, "schema changed", "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := probePeer(t, tc.fault)
			evidence, err := c.Check(t.Context(), tc.probe)
			status := "pass"
			if err != nil {
				status = "fail"
				var unavailable *UnavailableError
				if errors.As(err, &unavailable) {
					status = "inconclusive"
				}
			}
			if status != tc.want {
				t.Fatalf("status=%s want=%s evidence=%+v err=%v", status, tc.want, evidence, err)
			}
			if status == "pass" && evidence.PositiveStatus != 200 {
				t.Fatal("passed without positive control")
			}
		})
	}
}
