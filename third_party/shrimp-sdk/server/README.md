# Implementing an application adapter

Implement `server.Application` in the application that owns the accounts and
database. The [SDK setup guide](../README.md) covers HTTP configuration, trusted
enrollment and supported operations. This guide explains the commit boundary and
the reusable tests for that boundary.

## Commit and recovery order

Within the application's transaction, find a retained operation by its trusted
principal, replay window and operation ID. Compare the exact intent before
checking current revision, write permission or profile support. Return an exact
retry's immutable result; refuse a different intent with `ErrReplayConflict`.
Only an operation without a retained result may proceed to current validation.
`RecoverOnly` and `UnsupportedProfiles` must refuse new work without preventing
that retained lookup. A retained result must never be rebuilt from today's
account state.
Retained lookup never bypasses current authentication, enrollment or result
visibility checks; knowing an operation ID or token grants no authority.

Commit the native account change, owned protocol facts, subject revision, retained
result and required audit records together. The audit context from `BeginAudit`
must reach `Apply`; `IdentifyAudit` binds the attempt to the operation, and
`FinishAudit` must preserve the outcome already journaled by the transaction.
If the adapter projects a protocol name onto a native account field, both values
belong in this same commit.

Native writes and login or credential publication must use the same admission
fence: the application's mechanism that prevents a disable from racing with a
new login. A successful disable's blocking effect must already hold at
`Result.Time`. A later callback cannot establish this guarantee. Retain the
original lifecycle completion evidence so a retry after reactivation does not
claim a new disable or change the current account.

Use documented `server.Err…` sentinels, optionally wrapped with `%w`, for known
failures. Retained business rejections use public codes in `Result.Error`. An
unclassified storage error leaves the commit outcome unknown; never translate it
to a successful receipt or a known non-commit. The client can then recover using
the original operation identity.

## Run the bounded adapter tests

Import `github.com/lovablelabs/shrimp-protocol/sdk/go/server/servertest` from the
application's test package. Supply fresh, disposable, enrolled storage for every
fixture; the kit supplies no database. The following is an integration sketch:
`newTestApplication` and `closeAndReopenTestApplication` are host-owned helpers.

```go
func TestSHRIMPApplication(t *testing.T) {
    servertest.Run(t, func(t *testing.T) servertest.Fixture {
        app := newTestApplication(t) // Register storage cleanup with t.Cleanup.
        return servertest.Fixture{
            Application: app,
            Principal:   "test-provider",
            Authority:   "test-authority",
            Restart: func(t *testing.T) server.Application {
                return closeAndReopenTestApplication(t, app)
            },
            AtomicSubjectUpdates: true,
        }
    })
}
```

The fixture must support compatibility scalar create/update, with owned
`displayName` facts, plus activate/disable. Use the actual adapter and account
transaction. `Restart` must close and reopen the same durable state and preserve
enrollment. A nil restart hook explicitly skips its case. Set
`AtomicSubjectUpdates` only when the adapter supports an attribute change and
lifecycle transition in one operation; otherwise that case is explicitly skipped.

| Subtest | Checked behavior |
|---|---|
| `immutable_replay` | Old results stay exact after later revisions, write withdrawal and unsupported-profile flags; conflicting reuse fails; those flags refuse new work. |
| `failed_precondition` | A stale revision produces a retained rejection and leaves the current facts and lifecycle unchanged. |
| `restart_recovery` | A reopened adapter recovers the original result and replays it without reverting the later state. |
| `compound_update` | A failed scalar field leaves lifecycle unchanged; a valid attribute/lifecycle update commits together, and later replay preserves its original completion evidence. |

Run the host test with `go test -race ./path/to/adapter/...`. Inspect skips as
missing evidence, not successful checks. The kit calls `Window`, the mutation
audit methods, `Apply`, `Result` and causal `Read` directly. It compares complete
retained results, including the inputs used to produce lifecycle effect receipts.

These are selected application-interface checks. They do not exercise HTTP
authentication, discovery, enumeration, all profile semantics, arbitrary process
crashes or native admission. Keep application tests that observe real account
projection, audit records, paused and fresh logins, and credential publication
through response flush. The SDK's configuration opt-ins should reflect that real
application evidence. Passing the kit does not establish a complete baseline or
profile conformance claim.
