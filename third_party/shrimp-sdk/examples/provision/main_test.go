//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client"
)

func TestParseOptionsResumeRejectsNewDecisionArguments(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"recover", "retry"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			base := []string{"-profile", "profile.json", "-job", "saved-job", "-action", action}
			if _, err := parseOptions(base, io.Discard); err != nil {
				t.Fatalf("valid resume arguments: %v", err)
			}
			for _, flag := range []string{"authority", "source", "subject", "revision", "display-name", "department", "email", "clear"} {
				t.Run(flag, func(t *testing.T) {
					// Even an explicitly empty value must not be silently ignored.
					for _, value := range []string{"replacement", ""} {
						args := append(append([]string(nil), base...), "-"+flag, value)
						if _, err := parseOptions(args, io.Discard); err == nil || !strings.Contains(err.Error(), "not accepted") {
							t.Fatalf("-%s=%q error = %v; want rejected resume argument", flag, value, err)
						}
					}
				})
			}
		})
	}
}

func TestParseOptionsRequiresExplicitDecisionInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		args  []string
		valid bool
	}{
		{"create_empty_name_is_explicit", []string{"-action", "create", "-job", "job", "-authority", "hr", "-source", "source-1", "-display-name", ""}, true},
		{"create_missing_name", []string{"-action", "create", "-job", "job", "-authority", "hr", "-source", "source-1"}, false},
		{"update_empty_email_is_explicit", []string{"-action", "update", "-job", "job", "-authority", "hr", "-subject", "s1", "-revision", "r1", "-email", ""}, true},
		{"update_no_changes", []string{"-action", "update", "-job", "job", "-authority", "hr", "-subject", "s1", "-revision", "r1"}, false},
		{"activate_observed_revision", []string{"-action", "activate", "-job", "job", "-authority", "hr", "-subject", "s1", "-revision", "r1"}, true},
		{"activate_no_revision", []string{"-action", "activate", "-job", "job", "-authority", "hr", "-subject", "s1"}, false},
		{"disable_rejects_ignored_change", []string{"-action", "disable", "-job", "job", "-authority", "hr", "-subject", "s1", "-revision", "r1", "-email", "new@example.test"}, false},
		{"read_exact_subject", []string{"-action", "read", "-subject", "s1"}, true},
		{"read_rejects_job", []string{"-action", "read", "-subject", "s1", "-job", "job"}, false},
		{"resume_requires_job", []string{"-action", "retry"}, false},
		{"positional_rejected", []string{"-action", "recover", "-job", "job", "unexpected"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"-profile", "profile.json"}, tc.args...)
			if _, err := parseOptions(args, io.Discard); (err == nil) != tc.valid {
				t.Fatalf("parseOptions error = %v; valid = %v", err, tc.valid)
			}
		})
	}
}

func TestReportRequiresSucceededOutcome(t *testing.T) {
	t.Parallel()
	terminal := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	token := "causal-frontier"
	committed := client.ReceiptCommit{
		State: client.CommitCommitted, CausalToken: &token,
		Resources: []client.CommittedResource{{CommandID: "c1", Resource: client.ResourceRef{Type: "subject", ID: "s1"}, Revision: "r2"}},
	}
	cases := []struct {
		name    string
		receipt *client.Receipt
		state   client.OutcomeState
		valid   bool
	}{
		{"unknown", nil, client.OutcomeUnknown, false},
		{"pending", &client.Receipt{State: client.OutcomePending, Commit: committed}, client.OutcomePending, false},
		{"failed_after_commit", &client.Receipt{State: client.OutcomeFailed, Commit: committed, Error: &client.ReceiptError{Code: "enforcement_failed"}}, client.OutcomeFailed, false},
		{"failed_before_commit", &client.Receipt{State: client.OutcomeFailed, Commit: client.ReceiptCommit{State: client.CommitNotCommitted}}, client.OutcomeFailed, false},
		{"superseded", &client.Receipt{State: client.OutcomeSuperseded, Commit: committed}, client.OutcomeSuperseded, false},
		{"inconsistent_success", &client.Receipt{State: client.OutcomeSucceeded, Commit: client.ReceiptCommit{State: client.CommitUnknown}}, client.OutcomeUnknown, false},
		{"succeeded", &client.Receipt{State: client.OutcomeSucceeded, Commit: committed, TerminalAt: &terminal}, client.OutcomeSucceeded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := report(&out, client.Intent{}, client.Outcome{Receipt: tc.receipt}, nil)
			if (err == nil) != tc.valid {
				t.Fatalf("report error = %v; success = %v", err, tc.valid)
			}
			var summary struct {
				Outcome   client.OutcomeState `json:"outcome"`
				Commit    client.CommitState  `json:"commit"`
				ErrorCode string              `json:"error_code"`
			}
			if err := json.Unmarshal(out.Bytes(), &summary); err != nil {
				t.Fatal(err)
			}
			if summary.Outcome != tc.state {
				t.Errorf("reported outcome = %s; want %s", summary.Outcome, tc.state)
			}
			if tc.receipt != nil && summary.Commit != tc.receipt.Commit.State {
				t.Errorf("reported commit = %s; want %s", summary.Commit, tc.receipt.Commit.State)
			}
			if tc.receipt != nil && tc.receipt.Error != nil && summary.ErrorCode != tc.receipt.Error.Code {
				t.Errorf("recorded failure code was lost: %s", summary.ErrorCode)
			}
		})
	}
}

func TestReportPreservesCallError(t *testing.T) {
	t.Parallel()
	want := errors.New("receipt connection lost")
	if err := report(io.Discard, client.Intent{}, client.Outcome{}, want); !errors.Is(err, want) {
		t.Fatalf("report error = %v; want wrapped transport error", err)
	}
}

func TestResumeValidatesSavedIntentBeforeNetwork(t *testing.T) {
	// This test changes the default transport, so it must not run in parallel.
	var calls atomic.Int32
	previous := http.DefaultTransport
	transport := previous.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network call")
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	profilePath := testProfile(t)
	profile, err := client.ReadProfile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := client.New(profile)
	if err != nil {
		t.Fatalf("test enrollment is unusable: %v", err)
	}
	defer peer.Close()
	malformed := []byte("{\"request\":")
	_, restoreErr := peer.RestoreIntent(malformed)
	if restoreErr == nil {
		t.Fatal("malformed test intent was accepted")
	}
	for _, action := range []string{"recover", "retry"} {
		t.Run(action, func(t *testing.T) {
			j, path := freshJob(t)
			if err := j.save(context.Background(), "operation", malformed); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := run(context.Background(), []string{"-profile", profilePath, "-job", path, "-action", action}, &out, io.Discard)
			if err == nil || err.Error() != restoreErr.Error() {
				t.Fatalf("run error = %v; want saved intent rejection %v", err, restoreErr)
			}
			if calls.Load() != 0 || out.Len() != 0 {
				t.Fatalf("invalid job caused network calls (%d) or output (%q)", calls.Load(), out.String())
			}
			got, err := j.load()
			if err != nil || !bytes.Equal(got, malformed) {
				t.Fatalf("invalid saved job was modified: %v", err)
			}
		})
	}
}

func testProfile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	profile := client.Profile{
		Resource: "https://app.example.test/shrimp", Issuer: "https://issuer.example.test",
		Tenant: "tenant", Domain: "domain", ClientID: "client", ClientKeyID: "key",
		ClientKeyFile: keyPath, DPoPKeyFile: keyPath, Version: "0.2",
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
