package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Client has one enrolled identity and in-memory credentials for one invocation.
// It is not safe for concurrent use: issuer and resource nonce contexts are mutable.
type Client struct {
	profile              Profile
	http                 *http.Client
	key, proofKey        *ecdsa.PrivateKey
	token, resourceNonce string
	local, catalog       map[string]*jsonschema.Schema
	versions             map[string]bool
	schemaDigests        map[string]string
	selected             *contract
	writeScope           bool
}

// New validates credentials and TLS configuration without making network requests.
func New(p Profile) (*Client, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	key, err := loadKey(p.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("client authentication key: %w", err)
	}
	proof, err := loadKey(p.DPoPKeyFile)
	if err != nil {
		return nil, fmt.Errorf("DPoP key: %w", err)
	}
	var roots *x509.CertPool
	if p.CAFile != "" {
		data, err := readFile(p.CAFile, 1048576, false)
		if err != nil {
			return nil, fmt.Errorf("read CA file: %w", err)
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return nil, errors.New("CA file contains no certificates")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	transport.MaxResponseHeaderBytes = 32768
	transport.ResponseHeaderTimeout = 7 * time.Second
	client := &http.Client{Transport: transport, Timeout: 7 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	local, err := localSchemas()
	if err != nil {
		return nil, fmt.Errorf("embedded schema compilation failed: %w", err)
	}
	p.RequiredProfiles = slices.Clone(p.RequiredProfiles)
	return &Client{profile: p, http: client, key: key, proofKey: proof, local: local}, nil
}

// Close releases idle connections. Tokens are never written to disk.
func (c *Client) Close() { c.http.CloseIdleConnections() }

type response struct {
	status int
	header http.Header
	raw    []byte
	value  map[string]any
}

func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

func metadataURL(base, suffix string) string {
	u, _ := url.Parse(base)
	return origin(u) + "/.well-known/" + suffix + u.Path
}

var errResponseLimit = errors.New("HTTPS response exceeds client byte limit")

func (c *Client) request(ctx context.Context, method, endpoint, body string, headers http.Header, maximum int64) (response, error) {
	u, err := trustedURL(endpoint)
	if err != nil {
		return response{}, err
	}
	r, _ := url.Parse(c.profile.Resource)
	i, _ := url.Parse(c.profile.Issuer)
	if origin(u) != origin(r) && origin(u) != origin(i) {
		return response{}, errors.New("request leaves enrolled endpoint origins")
	}
	// Apply the same explicit bootstrap to ordinary discovery and its probes.
	if endpoint == c.profile.Resource+"/capabilities" && c.profile.Version == "0.2" {
		headers.Set("SHRIMP-Discovery-Version", "0.2")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return response{}, errors.New("cannot construct HTTP request")
	}
	req.Header = headers
	resp, err := c.http.Do(req)
	if err != nil {
		// Errors and server-controlled response bodies must not echo credentials.
		if ctx.Err() != nil {
			return response{}, ctx.Err()
		}
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return response{}, unavailable("TLS certificate verification failed; check endpoint and ca_file")
		}
		return response{}, unavailable("HTTPS request failed; check endpoint reachability and TLS configuration")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return response{}, unavailable("endpoint unavailable (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return response{}, errors.New("endpoint redirect refused; update the enrolled configuration explicitly")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maximum+1))
	if err != nil {
		return response{}, unavailable("cannot read complete HTTPS response")
	}
	if int64(len(raw)) > maximum {
		return response{}, errResponseLimit
	}
	v, err := decodeJSONLimit(raw, maximum)
	if err != nil {
		return response{}, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return response{}, errors.New("expected a JSON object response")
	}
	return response{resp.StatusCode, resp.Header, raw, obj}, nil
}

func contains(value any, wanted string) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if item == wanted {
			return true
		}
	}
	return false
}

func media(h http.Header, want string) bool {
	if len(h.Values("Content-Type")) != 1 {
		return false
	}
	value, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && value == want
}

func noStore(h http.Header) bool {
	for _, value := range h.Values("Cache-Control") {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "no-store") {
				return true
			}
		}
	}
	return false
}

func (c *Client) metadata(ctx context.Context, endpoint string) (map[string]any, error) {
	r, err := c.request(ctx, "GET", endpoint, "", make(http.Header), 65536)
	if err != nil {
		return nil, err
	}
	if r.status != 200 || !media(r.header, "application/json") {
		return nil, fmt.Errorf("OAuth metadata unavailable (HTTP %d)", r.status)
	}
	return r.value, nil
}

// Authenticate performs metadata checks before sending a signed assertion. Only
// shrimp.read is requested; registration and target delegation are administrative.
func (c *Client) Authenticate(ctx context.Context) error {
	return c.authenticate(ctx, false)
}

// AuthenticateLifecycle explicitly requests write authority for the isolated
// lifecycle suite. Normal diagnostics and app-read continue to request read only.
func (c *Client) AuthenticateLifecycle(ctx context.Context) error {
	return c.authenticate(ctx, true)
}

func (c *Client) authenticate(ctx context.Context, write bool) error {
	c.token, c.writeScope = "", false
	c.selected, c.catalog, c.versions, c.schemaDigests = nil, nil, nil, nil
	scope := "shrimp.read"
	if write {
		scope += " shrimp.write"
	}
	resource, err := c.metadata(ctx, metadataURL(c.profile.Resource, "oauth-protected-resource"))
	if err != nil {
		return fmt.Errorf("resource metadata: %w", err)
	}
	bearer, ok := resource["bearer_methods_supported"].([]any)
	if resource["resource"] != c.profile.Resource || !contains(resource["authorization_servers"], c.profile.Issuer) ||
		!contains(resource["scopes_supported"], "shrimp.read") || !ok || len(bearer) != 0 {
		return errors.New("resource metadata differs from enrolled resource/issuer or required read/DPoP profile")
	}
	if write && !contains(resource["scopes_supported"], "shrimp.write") {
		return unavailable("resource does not advertise write scope")
	}
	issuer, err := c.metadata(ctx, metadataURL(c.profile.Issuer, "oauth-authorization-server"))
	if err != nil {
		return fmt.Errorf("issuer metadata: %w", err)
	}
	if issuer["issuer"] != c.profile.Issuer || !contains(issuer["grant_types_supported"], "client_credentials") ||
		!contains(issuer["token_endpoint_auth_methods_supported"], "private_key_jwt") ||
		!contains(issuer["token_endpoint_auth_signing_alg_values_supported"], "ES256") ||
		!contains(issuer["dpop_signing_alg_values_supported"], "ES256") {
		return errors.New("issuer metadata does not match enrolled issuer and required authentication profile")
	}
	if resources, present := issuer["protected_resources"]; present && !contains(resources, c.profile.Resource) {
		return errors.New("issuer does not advertise the enrolled resource")
	}
	endpoint, _ := issuer["token_endpoint"].(string)
	u, err := trustedURL(endpoint)
	if err != nil {
		return errors.New("issuer advertised an invalid token endpoint")
	}
	i, _ := url.Parse(c.profile.Issuer)
	if origin(u) != origin(i) {
		return errors.New("token endpoint leaves enrolled issuer origin; no credential was sent")
	}
	nonce := ""
	for attempt := 0; attempt < 2; attempt++ {
		now := time.Now().Unix()
		assertion, err := sign(c.key, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", c.profile.ClientKeyID),
			map[string]any{"iss": c.profile.ClientID, "sub": c.profile.ClientID, "aud": endpoint,
				"iat": now, "exp": now + 60, "jti": rand.Text()})
		if err != nil {
			return err
		}
		proof, err := c.proof("POST", endpoint, nonce, "")
		if err != nil {
			return err
		}
		form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.profile.ClientID},
			"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
			"client_assertion":      {assertion}, "resource": {c.profile.Resource}, "scope": {scope}}
		headers := make(http.Header)
		headers.Set("Content-Type", "application/x-www-form-urlencoded")
		headers.Set("DPoP", proof)
		r, err := c.request(ctx, "POST", endpoint, form.Encode(), headers, 65536)
		if err != nil {
			return err
		}
		if !media(r.header, "application/json") || !noStore(r.header) {
			return errors.New("token response must be JSON with Cache-Control: no-store")
		}
		if r.status == 400 && r.value["error"] == "use_dpop_nonce" && attempt == 0 {
			nonce, err = responseNonce(r.header)
			if err != nil {
				return err
			}
			continue
		}
		if r.status != 200 {
			return unavailable("requested-scope authentication refused (HTTP %d); verify enrollment and clock before testing", r.status)
		}
		expires, ok := r.value["expires_in"].(json.Number)
		seconds, e := expires.Int64()
		token, _ := r.value["access_token"].(string)
		_, refresh := r.value["refresh_token"]
		if !ok || e != nil || seconds < 1 || seconds > 300 || r.value["token_type"] != "DPoP" ||
			!sameScopes(r.value["scope"], scope) || refresh || token == "" || strings.ContainsAny(token, " \t\r\n") ||
			!strings.EqualFold(r.header.Get("Pragma"), "no-cache") {
			return errors.New("token response violates the requested SHRIMP authentication profile")
		}
		c.token = token
		c.writeScope = write
		return nil
	}
	return errors.New("token nonce retry limit exceeded")
}

func sameScopes(value any, wanted string) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	got, want := strings.Split(s, " "), strings.Split(wanted, " ")
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

func sign(key *ecdsa.PrivateKey, options *jose.SignerOptions, claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, options)
	if err != nil {
		return "", errors.New("cannot initialize ES256 signer")
	}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signed, err := signer.Sign(data)
	if err != nil {
		return "", errors.New("ES256 signing failed")
	}
	return signed.CompactSerialize()
}

func (c *Client) proof(method, endpoint, nonce, token string) (string, error) {
	claims := map[string]any{"htm": method, "htu": endpoint, "iat": time.Now().Unix(), "jti": rand.Text()}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if token != "" {
		hash := sha256.Sum256([]byte(token))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(hash[:])
	}
	options := (&jose.SignerOptions{}).WithType("dpop+jwt").WithHeader("jwk", jose.JSONWebKey{Key: &c.proofKey.PublicKey})
	return sign(c.proofKey, options, claims)
}

func responseNonce(h http.Header) (string, error) {
	nonce := h.Get("DPoP-Nonce")
	if len(h.Values("DPoP-Nonce")) != 1 || nonce == "" || len(nonce) > 4096 || strings.ContainsAny(nonce, " ,\r\n\t") {
		return "", errors.New("missing or invalid DPoP nonce challenge")
	}
	return nonce, nil
}

func (c *Client) get(ctx context.Context, path, version string) (response, error) {
	return c.api(ctx, "GET", path, version, "")
}

func (c *Client) api(ctx context.Context, method, path, version, body string) (response, error) {
	maximum := int64(262144)
	if path == "/capabilities" {
		maximum = 65536
	}
	return c.apiWithLimit(ctx, method, path, version, body, maximum)
}

func (c *Client) apiWithLimit(ctx context.Context, method, path, version, body string, maximum int64) (response, error) {
	if c.token == "" {
		return response{}, errors.New("authenticate before requesting protected data")
	}
	ctx, cancel := context.WithTimeout(ctx, 14*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := c.proof(method, c.profile.Resource+path, c.resourceNonce, c.token)
		if err != nil {
			return response{}, err
		}
		headers := make(http.Header)
		headers.Set("Authorization", "DPoP "+c.token)
		headers.Set("DPoP", proof)
		if body != "" {
			headers.Set("Content-Type", "application/json")
		}
		if version != "" {
			headers.Set("SHRIMP-Version", version)
		}
		r, err := c.request(ctx, method, c.profile.Resource+path, body, headers, maximum)
		if err != nil {
			return response{}, err
		}
		if !noStore(r.header) {
			return response{}, errors.New("protected response permits caching")
		}
		if r.status == 401 && r.value["code"] == "use_dpop_nonce" && attempt == 0 {
			if !strings.HasPrefix(r.header.Get("WWW-Authenticate"), "DPoP ") {
				return response{}, errors.New("nonce response omits DPoP authentication challenge")
			}
			c.resourceNonce, err = responseNonce(r.header)
			if err != nil {
				return response{}, err
			}
			continue
		}
		if r.status < 400 && version != "" && (len(r.header.Values("SHRIMP-Version")) != 1 || r.header.Get("SHRIMP-Version") != version) {
			return response{}, errors.New("response omitted or changed the selected SHRIMP version")
		}
		if r.status < 400 && path == "/capabilities" && !c.discoveryVersion(r.header) {
			return response{}, errors.New("response omitted or changed the selected discovery version")
		}
		if r.status >= 400 {
			if !c.validProblem(r) {
				return response{}, errors.New("invalid SHRIMP problem response")
			}
		}
		return r, nil
	}
	return response{}, errors.New("resource nonce retry limit exceeded")
}

// Inspect always queries the original version/window/ID under current authority.
// A failed lookup never establishes that an earlier mutation did not commit.
func (c *Client) Inspect(ctx context.Context, window, id string) (map[string]any, error) {
	if !identifier.MatchString(window) || !identifier.MatchString(id) {
		return nil, errors.New("valid --replay-window and operation ID are required")
	}
	path := "/operations/" + window + "/" + id
	r, err := c.get(ctx, path, c.profile.Version)
	if err != nil {
		return nil, fmt.Errorf("receipt unavailable; prior outcome remains unknown: %w", err)
	}
	if r.status != 200 {
		return nil, unavailable("receipt unavailable (HTTP %d); prior outcome remains unknown", r.status)
	}
	if !media(r.header, "application/json") {
		return nil, errors.New("receipt must use application/json")
	}
	valid := false
	for _, name := range c.receiptSchemas() {
		// New-work catalogs cannot change the decoder for a retained historical
		// result, even when discovery happened earlier in this same invocation.
		if c.local[name].Validate(r.value) == nil {
			valid = true
		}
	}
	if !valid {
		return nil, errors.New("receipt does not match a supported receipt schema")
	}
	op, _ := r.value["operation"].(map[string]any)
	if c.profile.Version == "0.2" {
		if op["id"] != id || op["replay_window"] != window {
			return nil, errors.New("receipt identifies a different operation")
		}
		if err := c.validateReceipt02(r.value); err != nil {
			return nil, err
		}
		return r.value, nil
	}
	u, _ := url.Parse(c.profile.Resource)
	if op["id"] != id || op["replay_window"] != window || op["href"] != u.Path+path {
		return nil, errors.New("receipt identifies a different operation or tenant/domain path")
	}
	return r.value, nil
}

// Diagnostics reports limited client checks, never a conformance verdict.
type Diagnostics struct {
	Resource         string            `json:"resource"`
	Issuer           string            `json:"issuer"`
	Scope            map[string]string `json:"scope"`
	Version          string            `json:"version"`
	Checks           []string          `json:"checks"`
	Operations       []string          `json:"operations"`
	Profiles         []string          `json:"profiles"`
	RequiredProfiles []string          `json:"required_profiles"`
	Untested         []string          `json:"untested"`
}

func (c *Client) diagnostics(operations, profiles []string) Diagnostics {
	return Diagnostics{Resource: c.profile.Resource, Issuer: c.profile.Issuer,
		Scope: map[string]string{"tenant": c.profile.Tenant, "domain": c.profile.Domain}, Version: c.profile.Version,
		Checks: []string{"tls", "oauth_read_dpop", "discovery", "schema_integrity"}, Operations: slices.Clone(operations),
		Profiles: slices.Clone(profiles), RequiredProfiles: c.profile.DiscoveryProfiles(),
		Untested: []string{"application_admission", "provisioning_mutations", "independent_interoperability", "full_core"}}
}

func (c *Client) discoveryVersion(h http.Header) bool {
	return c.profile.Version != "0.2" || len(h.Values("SHRIMP-Discovery-Version")) == 1 && h.Get("SHRIMP-Discovery-Version") == "0.2"
}

func (c *Client) validProblem(r response) bool {
	if !media(r.header, "application/problem+json") || r.value["status"] != json.Number(fmt.Sprint(r.status)) {
		return false
	}
	if c.local[c.schemaName("problem")].Validate(r.value) == nil {
		return true
	}
	// A dual-version target can reject an absent/unsupported version before
	// choosing a decoder. This exception accepts only a negotiation rejection,
	// never a legacy success, receipt, or authenticated 0.2 operation error.
	if c.profile.Version == "0.2" && r.status == 400 && len(r.header.Values("SHRIMP-Version")) == 0 &&
		(r.value["code"] == "version_required" || r.value["code"] == "unsupported_version" || r.value["code"] == "unsupported_discovery_version") {
		return c.local["problem.schema.json"].Validate(r.value) == nil
	}
	return false
}
