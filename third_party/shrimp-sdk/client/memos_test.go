package client_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client"
)

// This test is opt-in and only for a disposable Memos fixture started by the pilot runner.
func TestMemosProvisioning(t *testing.T) {
	path := os.Getenv("SHRIMP_DISPOSABLE_MEMOS_CONFIG")
	if path == "" {
		t.Skip("requires disposable Memos pilot")
	}
	p, err := client.ReadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	connect := func() *client.Client {
		c, err := client.New(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Close)
		if err := c.AuthenticateWrite(t.Context()); err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := connect()
	if _, err := c.Discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	save := func(_ context.Context, id string, raw []byte) error {
		path := filepath.Join(dir, id+".json")
		if old, err := os.ReadFile(path); err == nil {
			if !bytes.Equal(old, raw) {
				t.Fatal("conflicting intent")
			}
			return nil
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(raw); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		directory, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer directory.Close()
		return directory.Sync()
	}
	email, department := "SDK+Pilot@Example.COM", "Research"
	create, err := c.PrepareCreate(t.Context(), client.Human{Authority: "hr-authority", SourceReference: rand.Text(), DisplayName: "SDK pilot", Email: &email, Department: &department})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := c.Submit(t.Context(), create, save)
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, v := range receipt["commit"].(map[string]any)["resources"].([]any) {
		r := v.(map[string]any)["resource"].(map[string]any)
		if r["type"] == "subject" {
			id = r["id"].(string)
		}
	}
	if id == "" {
		t.Fatal("missing subject")
	}
	read := func(want string) client.SubjectVersion {
		r, err := c.ReadSubject(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r["value"].(map[string]any)["lifecycle"] != want {
			t.Fatalf("wanted %s", want)
		}
		return client.SubjectVersion{ID: id, Revision: r["revision"].(string), Authority: r["authority"].(string)}
	}
	activate, err := c.PrepareActivate(t.Context(), read("disabled"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(t.Context(), activate, save); err != nil {
		t.Fatal(err)
	}
	update, err := c.PrepareUpdate(t.Context(), read("active"), client.ScalarChanges{Set: map[string]string{"department": ""}, Clear: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(t.Context(), update, save); err != nil {
		t.Fatal(err)
	}
	r, err := c.ReadSubject(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	facts := r["value"].(map[string]any)["attributes"].(map[string]any)
	if facts["email"].(map[string]any)["value"] != nil || facts["department"].(map[string]any)["value"] != "" || facts["displayName"].(map[string]any)["value"] != "SDK pilot" {
		t.Fatalf("update did not preserve null, empty and omitted facts: %v", facts)
	}
	disable, err := c.PrepareDisable(t.Context(), read("active"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(t.Context(), disable, save); err != nil {
		t.Fatal(err)
	}
	read("disabled")
	restarted := connect() // No new-work discovery needed for recovery or exact retry.
	raw, err := os.ReadFile(filepath.Join(dir, update.ID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.RestoreIntent(raw)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Recover(t.Context(), restored)
	if err != nil || recovered["state"] != "succeeded" {
		t.Fatalf("recover: %v", err)
	}
	if _, err := restarted.Submit(t.Context(), restored, save); err != nil {
		t.Fatal(err)
	}
	read("disabled")
}
