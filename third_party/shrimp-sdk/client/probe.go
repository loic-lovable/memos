package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
)

// Probe identifies a read-only negative test with a successful control request.
type Probe string

const (
	Unauthenticated Probe = "unauthenticated"
	MissingVersion  Probe = "missing_version"
	UnknownVersion  Probe = "unknown_version"
	ReplayedProof   Probe = "replayed_proof"
)

// ProbeEvidence excludes credentials, proofs, nonces, and response bodies.
type ProbeEvidence struct {
	PositiveStatus int `json:"positive_http_status"`
	NegativeStatus int `json:"negative_http_status"`
}

// Check exercises only GET routes on the enrolled resource. The replay probe is
// the sole deliberate reuse of a DPoP proof; ordinary client requests stay fresh.
func (c *Client) Check(ctx context.Context, probe Probe) (ProbeEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var evidence ProbeEvidence
	if c.catalog == nil {
		return evidence, unavailable("successful discovery is required before protocol probes")
	}
	path := "/capabilities"
	version := ""
	if probe == MissingVersion || probe == UnknownVersion {
		name := ""
		for _, candidate := range c.receiptSchemas() {
			if c.catalog[candidate] != nil {
				name = candidate
				break
			}
		}
		if name == "" {
			return evidence, unavailable("no supported receipt schema selected")
		}
		path, version = "/schemas/"+name, c.profile.Version
	}
	control, err := c.get(ctx, path, version)
	if err != nil {
		return evidence, err
	}
	evidence.PositiveStatus = control.status
	if control.status != 200 {
		return evidence, unavailable("positive control unavailable (HTTP %d)", control.status)
	}
	if err := c.validateControl(path, control); err != nil {
		return evidence, err
	}
	var negative response
	wantStatus, wantCode := 400, ""
	switch probe {
	case Unauthenticated:
		negative, err = c.request(ctx, "GET", c.profile.Resource+path, "", make(http.Header), 65536)
		wantStatus = 401
	case MissingVersion:
		negative, err = c.get(ctx, path, "")
		wantCode = "version_required"
	case UnknownVersion:
		unsupported := "shrimp-test-" + rand.Text()
		for c.versions[unsupported] {
			unsupported = "shrimp-test-" + rand.Text()
		}
		negative, err = c.get(ctx, path, unsupported)
		wantCode = "unsupported_version"
	case ReplayedProof:
		negative, err = c.replay(ctx, path)
		wantStatus, wantCode = 401, "invalid_dpop_proof"
	default:
		return evidence, errors.New("unknown protocol probe")
	}
	if err != nil {
		return evidence, err
	}
	evidence.NegativeStatus = negative.status
	if probe != Unauthenticated && (negative.status == 403 || negative.status == 401 && negative.value["code"] != wantCode) {
		return evidence, unavailable("probe interrupted by an authority or nonce change (HTTP %d)", negative.status)
	}
	if negative.status != wantStatus {
		return evidence, fmt.Errorf("negative request returned HTTP %d; expected %d", negative.status, wantStatus)
	}
	if !noStore(negative.header) || !c.validProblem(negative) {
		return evidence, errors.New("negative response violates the problem-details or no-store contract")
	}
	if wantStatus == 401 && !strings.HasPrefix(negative.header.Get("WWW-Authenticate"), "DPoP ") {
		return evidence, errors.New("authentication rejection omitted its DPoP challenge")
	}
	if wantCode != "" && negative.value["code"] != wantCode {
		return evidence, errors.New("negative response did not identify the tested rejection")
	}
	if wantStatus == 400 && negative.header.Get("SHRIMP-Version") != "" {
		return evidence, errors.New("missing or unsupported version was echoed as selected")
	}
	return evidence, nil
}

func (c *Client) replay(ctx context.Context, path string) (response, error) {
	// A proof must first succeed, including a bounded nonce challenge if needed.
	// Rejection of a proof that never succeeded is not replay-prevention evidence.
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := c.proof("GET", c.profile.Resource+path, c.resourceNonce, c.token)
		if err != nil {
			return response{}, err
		}
		headers := make(http.Header)
		headers.Set("Authorization", "DPoP "+c.token)
		headers.Set("DPoP", proof)
		accepted, err := c.request(ctx, "GET", c.profile.Resource+path, "", headers, 65536)
		if err != nil {
			return response{}, err
		}
		if accepted.status == 401 && accepted.value["code"] == "use_dpop_nonce" && attempt == 0 {
			c.resourceNonce, err = responseNonce(accepted.header)
			if err != nil {
				return response{}, err
			}
			continue
		}
		if accepted.status != 200 {
			return response{}, unavailable("replay positive control unavailable (HTTP %d)", accepted.status)
		}
		if err := c.validateControl(path, accepted); err != nil {
			return response{}, err
		}
		// No nonce repair on the repeated proof. A new nonce/proof would erase the
		// condition being tested and could turn a broken target into a false pass.
		return c.request(ctx, "GET", c.profile.Resource+path, "", headers.Clone(), 65536)
	}
	return response{}, unavailable("no accepted proof available for replay test")
}

func (c *Client) validateControl(route string, r response) error {
	if !noStore(r.header) {
		return errors.New("positive control permits caching")
	}
	if route == "/capabilities" {
		if !media(r.header, "application/json") || !c.discoveryVersion(r.header) || c.local[c.schemaName("discovery")].Validate(r.value) != nil {
			return errors.New("positive control returned invalid discovery")
		}
		scope, _ := r.value["scope"].(map[string]any)
		if r.value["resource"] != c.profile.Resource || scope["tenant"] != c.profile.Tenant || scope["domain"] != c.profile.Domain {
			return errors.New("positive control returned another resource or tenant/domain")
		}
		return nil
	}
	if !media(r.header, "application/schema+json") || fmt.Sprintf("%x", sha256.Sum256(r.raw)) != c.schemaDigests[path.Base(route)] {
		return errors.New("positive control failed advertised schema integrity")
	}
	return nil
}
