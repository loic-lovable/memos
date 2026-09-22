// Package client implements an experimental enrolled SHRIMP client.
package client

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)

// Profile records explicitly enrolled trust and file references, never tokens or keys.
type Profile struct {
	Resource      string `json:"resource"`
	Issuer        string `json:"issuer"`
	Tenant        string `json:"tenant"`
	Domain        string `json:"domain"`
	ClientID      string `json:"client_id"`
	ClientKeyFile string `json:"client_key_file"`
	ClientKeyID   string `json:"client_key_id"`
	DPoPKeyFile   string `json:"dpop_key_file"`
	CAFile        string `json:"ca_file,omitempty"`
	Version       string `json:"version"`
	// Nil selects the 0.2 baseline/human default; an empty slice explicitly
	// permits discovery of a partial implementation. Preserve that distinction.
	RequiredProfiles []string `json:"required_profiles"`
}

// ReadProfile resolves relative file references against the configuration file.
func ReadProfile(path string) (Profile, error) {
	var p Profile
	raw, err := readFile(path, 65536, false)
	if err != nil {
		return p, fmt.Errorf("read connection config: %w", err)
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return p, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return p, errors.New("connection config must be a JSON object")
	}
	for name := range object {
		switch name {
		case "resource", "issuer", "tenant", "domain", "client_id", "client_key_file", "client_key_id", "dpop_key_file", "ca_file", "version", "required_profiles":
		default:
			return p, errors.New("unknown connection config field; use the exact names in docs/cli.md")
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, errors.New("invalid connection config fields; see docs/cli.md")
	}
	for _, value := range []*string{&p.ClientKeyFile, &p.DPoPKeyFile, &p.CAFile} {
		if *value != "" && !filepath.IsAbs(*value) {
			*value, err = filepath.Abs(filepath.Join(filepath.Dir(path), *value))
			if err != nil {
				return p, err
			}
		}
	}
	return p, p.Validate()
}

// Validate checks enrollment fields without reading keys or contacting a peer.
func (p Profile) Validate() error {
	for _, value := range []string{p.Resource, p.Issuer} {
		if _, err := trustedURL(value); err != nil {
			return err
		}
		if strings.HasSuffix(value, "/") {
			return errors.New("resource and issuer must have no trailing slash")
		}
	}
	for _, value := range []string{p.Tenant, p.Domain, p.ClientID, p.ClientKeyID} {
		if value == "" || len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("tenant, domain, client_id and client_key_id must be nonempty identifiers")
		}
	}
	if p.ClientKeyFile == "" || p.DPoPKeyFile == "" {
		return errors.New("client_key_file and dpop_key_file are required")
	}
	if p.Version != "0.1" && p.Version != "0.2" {
		return errors.New("this client supports only explicit version 0.1 or 0.2")
	}
	if p.Version == "0.1" && p.RequiredProfiles != nil {
		return errors.New("required_profiles needs version 0.2")
	}
	if len(p.RequiredProfiles) > 64 {
		return errors.New("required_profiles exceeds 64 entries")
	}
	seen := map[string]bool{}
	for _, name := range p.RequiredProfiles {
		if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 128 || seen[name] {
			return errors.New("required_profiles must contain unique names of 1–128 characters")
		}
		seen[name] = true
	}
	return nil
}

// DiscoveryProfiles returns the explicit requirements, or the 0.2 default.
// This selection applies to new work, never to retained operation recovery.
func (p Profile) DiscoveryProfiles() []string {
	if p.Version == "0.2" && p.RequiredProfiles == nil {
		return []string{"baseline", "human"}
	}
	return slices.Clone(p.RequiredProfiles)
}

func trustedURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(value, "#") ||
		u.Opaque != "" || strings.ContainsAny(value, "%\\\r\n\t ") || strings.Contains(u.Path, "//") {
		return nil, errors.New("connection endpoints must be HTTPS URLs without credentials, query, fragment or escaped path")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("connection endpoint contains a dot segment")
		}
	}
	return u, nil
}

func readFile(path string, limit int64, secret bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	if secret && runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("private key file must have mode 0600 or 0400")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return raw, nil
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	raw, err := readFile(path, 65536, true)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(raw)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected one unencrypted PEM private key")
	}
	var parsed any
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, errors.New("expected an unencrypted PKCS8 or EC private key")
	}
	if err != nil {
		return nil, errors.New("invalid private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("private key must be an ES256 (P-256) key")
	}
	return key, nil
}
