//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client"
)

// TestMemosProvisioningExample is opt-in: the pilot harness owns the disposable
// Memos server, enrollment, and cleanup. Every invocation creates a fresh client.
func TestMemosProvisioningExample(t *testing.T) {
	profile := os.Getenv("SHRIMP_DISPOSABLE_MEMOS_CONFIG")
	if profile == "" {
		t.Skip("requires disposable Memos pilot")
	}
	invoke := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		var stdout, stderr bytes.Buffer
		if err := run(ctx, append([]string{"-profile", profile}, args...), &stdout, &stderr); err != nil {
			t.Fatalf("provision %v: %v; stdout=%s stderr=%s", args, err, stdout.String(), stderr.String())
		}
		return stdout.Bytes()
	}
	type mutationResult struct {
		Outcome      client.OutcomeState        `json:"outcome"`
		OperationID  string                     `json:"operation_id"`
		ReplayWindow string                     `json:"replay_window"`
		Commit       client.CommitState         `json:"commit"`
		Resources    []client.CommittedResource `json:"resources"`
		Subject      client.CommittedResource   `json:"-"`
	}
	mutate := func(args ...string) mutationResult {
		t.Helper()
		var result mutationResult
		if err := json.Unmarshal(invoke(args...), &result); err != nil {
			t.Fatal(err)
		}
		if result.Outcome != client.OutcomeSucceeded || result.Commit != client.CommitCommitted ||
			result.OperationID == "" || result.ReplayWindow == "" || len(result.Resources) == 0 {
			t.Fatalf("missing successful operation evidence: %+v", result)
		}
		for _, resource := range result.Resources {
			if resource.Resource.Type == "subject" {
				if result.Subject.Resource.ID != "" {
					t.Fatalf("operation returned multiple subjects: %+v", result.Resources)
				}
				result.Subject = resource
			}
		}
		if result.Subject.Resource.ID == "" || result.Subject.Revision == "" {
			t.Fatalf("missing committed subject identity or revision: %+v", result.Resources)
		}
		return result
	}
	read := func(subject, revision, lifecycle string) {
		t.Helper()
		var result struct {
			Subject   string `json:"subject"`
			Revision  string `json:"revision"`
			Authority string `json:"authority"`
			Lifecycle string `json:"lifecycle"`
		}
		if err := json.Unmarshal(invoke("-action", "read", "-subject", subject), &result); err != nil {
			t.Fatal(err)
		}
		if result.Subject != subject || result.Revision != revision || result.Authority != "hr-authority" || result.Lifecycle != lifecycle {
			t.Fatalf("current subject = %+v; want %s revision %s, hr-authority, %s", result, subject, revision, lifecycle)
		}
	}
	newJobPath := func() string {
		t.Helper()
		return filepath.Join(privateParent(t), "decision")
	}
	create := mutate("-action", "create", "-job", newJobPath(), "-authority", "hr-authority",
		"-source", rand.Text(), "-display-name", "Provision example pilot", "-department", "Research")
	subject := create.Subject.Resource.ID
	read(subject, create.Subject.Revision, "disabled")

	update := mutate("-action", "update", "-job", newJobPath(), "-authority", "hr-authority",
		"-subject", subject, "-revision", create.Subject.Revision, "-department", "Engineering")
	activationJob := newJobPath()
	activate := mutate("-action", "activate", "-job", activationJob, "-authority", "hr-authority",
		"-subject", subject, "-revision", update.Subject.Revision)
	read(subject, activate.Subject.Revision, "active")

	disable := mutate("-action", "disable", "-job", newJobPath(), "-authority", "hr-authority",
		"-subject", subject, "-revision", activate.Subject.Revision)
	read(subject, disable.Subject.Revision, "disabled")
	seenOperations := map[string]bool{}
	seenRevisions := map[string]bool{}
	for _, result := range []mutationResult{create, update, activate, disable} {
		resource := result.Subject
		if resource.Resource.ID != subject || seenOperations[result.OperationID] || seenRevisions[resource.Revision] {
			t.Fatalf("new decision did not preserve subject identity and advance its operation/revision: %+v", result)
		}
		seenOperations[result.OperationID] = true
		seenRevisions[resource.Revision] = true
	}

	// The saved activation is now historical. Looking it up or resending its
	// exact request must return its old result, without activating the account.
	intentPath := filepath.Join(activationJob, "intent.json")
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"recover", "retry"} {
		result := mutate("-action", action, "-job", activationJob)
		if result.OperationID != activate.OperationID || result.ReplayWindow != activate.ReplayWindow ||
			!slices.Equal(result.Resources, activate.Resources) {
			t.Fatalf("%s changed the historical operation result: got %+v, want %+v", action, result, activate)
		}
		after, err := os.ReadFile(intentPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("%s replaced the saved intent: %v", action, err)
		}
		read(subject, disable.Subject.Revision, "disabled")
	}
}
