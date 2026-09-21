package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type recoveryFileSnapshot struct {
	Contents string
	Mode     os.FileMode
	Modified int64
}

func recoveryTree(t *testing.T, root string) map[string]recoveryFileSnapshot {
	t.Helper()
	result := map[string]recoveryFileSnapshot{}
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		value := recoveryFileSnapshot{Mode: info.Mode(), Modified: info.ModTime().UnixNano()}
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value.Contents = string(raw)
		}
		result[path] = value
		return nil
	}))
	return result
}

func TestRecoveryInspectionIsReadOnly(t *testing.T) {
	for _, state := range []string{"checkpoint_match", "checkpoint_mismatch", "uncertain", "missing_evidence", "invalid_evidence", "identity_mismatch", "lock_unavailable"} {
		t.Run(state, func(t *testing.T) {
			p, registry := recoveryFixture(t)
			g, err := beginRecovery(p, registry, true)
			require.NoError(t, err)
			if state != "uncertain" {
				require.NoError(t, g.seal())
			}
			if state != "lock_unavailable" {
				g.close()
			} else {
				defer g.close()
			}
			switch state {
			case "checkpoint_mismatch":
				require.NoError(t, os.WriteFile(p.DSN, []byte("restored private database"), 0600))
			case "missing_evidence":
				require.NoError(t, os.Remove(g.witness))
			case "invalid_evidence":
				require.NoError(t, os.WriteFile(g.witness, []byte(`{"secret":"must never be printed"}`), 0600))
			case "identity_mismatch":
				p.DSN = filepath.Join(p.Data, "wrong.db")
			}
			before := recoveryTree(t, filepath.Dir(p.Data))
			status, err := inspectRecovery(p.Data, p.DSN, registry)
			require.NoError(t, err)
			require.Equal(t, state, status.Status)
			require.False(t, status.RecoveryAuthorized)
			require.Equal(t, before, recoveryTree(t, filepath.Dir(p.Data)), "inspection must not create, replace, touch or change evidence")
			raw, err := json.Marshal(status)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "must never be printed")
			require.NotContains(t, string(raw), "private database")
			if state == "checkpoint_match" {
				require.NotNil(t, status.CheckpointMatches)
				require.True(t, *status.CheckpointMatches)
			}
			if state == "checkpoint_mismatch" {
				require.NotNil(t, status.CheckpointMatches)
				require.False(t, *status.CheckpointMatches)
			}
		})
	}
}

func TestRecoveryInspectionNoEvidenceDoesNotEnroll(t *testing.T) {
	p, registry := recoveryFixture(t)
	before := recoveryTree(t, filepath.Dir(p.Data))
	status, err := inspectRecovery(p.Data, "", registry)
	require.NoError(t, err)
	require.Equal(t, "no_evidence", status.Status)
	require.False(t, status.RecoveryAuthorized)
	require.Nil(t, status.CheckpointMatches)
	require.Equal(t, before, recoveryTree(t, filepath.Dir(p.Data)))
	_, err = inspectRecovery(p.DSN, "", registry)
	require.Error(t, err)
	_, err = inspectRecovery(filepath.Join(p.Data, "missing"), "", registry)
	require.Error(t, err)
	require.Equal(t, before, recoveryTree(t, filepath.Dir(p.Data)))
}

func TestRecoveryInspectionCommandRequiresExplicitData(t *testing.T) {
	command := newRecoveryStatusCommand()
	command.SetArgs(nil)
	require.ErrorContains(t, command.Execute(), "required flag")
}

func TestRecoveryInspectionCommandPrintsStructuredStatus(t *testing.T) {
	p, _ := recoveryFixture(t)
	command := newRecoveryStatusCommand()
	output := &bytes.Buffer{}
	command.SetOut(output)
	command.SetArgs([]string{"--data", p.Data})
	require.NoError(t, command.Execute())
	var status recoveryStatus
	require.NoError(t, json.Unmarshal(output.Bytes(), &status))
	require.Equal(t, "no_evidence", status.Status)
	require.False(t, strings.Contains(output.String(), p.Data), "report contains no filesystem inventory")
}
