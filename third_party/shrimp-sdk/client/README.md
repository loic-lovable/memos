# Experimental Go provisioning client

Use this package in a provisioning provider or bridge that creates, updates, activates and
disables human accounts in a SHRIMP application. The CLI now uses the same client
for its existing diagnostics, lifecycle and enumeration checks.

This is a bounded 0.2 API, not a complete provider framework or a production-ready
SDK. It does not include a SCIM bridge, source identity mapping, a database,
scheduling, or automatic reconciliation. A successful receipt is the target's
statement; application admission checks remain necessary evidence.

## Prepare, save, submit

Import `github.com/lovablelabs/shrimp-protocol/sdk/go/client` from the experimental
Go SDK module. The module is currently internal and unversioned; local consumers
can use a Go module `replace` pointing to `sdk/go`. Go 1.27 is required.

1. Load an explicitly enrolled `Profile` with `ReadProfile`, construct `New`, then
   call `AuthenticateWrite` and `Discover`. Credentials currently use private PEM
   files; custom CA trust is optional. HTTPS verification is mandatory.
2. Call `PrepareCreate`, `PrepareActivate` or `PrepareDisable`. Use `PrepareUpdate`
   for owned scalar changes. Preparation obtains a replay window but sends no mutation.
   Updates, activation and disable require the exact
   subject ID, revision and authority on which your decision was based.
3. Call `Submit(ctx, intent, save)`. Your `SaveIntent` callback must commit the
   complete bytes durably before returning nil. It must reject a conflicting
   replacement and accept identical retries. A nil or failed saver prevents sending.
4. Inspect the returned receipt's `state`, `commit`, and `effects`. Pending and
   failed results are not successful provisioning. A transport error can mean the
   change committed and its reply was lost.
5. After a restart, call `RestoreIntent(savedBytes)` and `Recover`. If appropriate,
   retry `Submit` with that same intent. Never prepare a replacement merely because
   a response or result lookup failed.

```go
intent, err := peer.PrepareCreate(ctx, client.Human{
    Authority:       "hr-authority",
    SourceReference: "employee-42",
    DisplayName:     "Alice",
})
if err != nil {
    return err
}
receipt, err := peer.Submit(ctx, intent, saveIntent)
// Keep the saved intent even if err != nil. The outcome may be unknown.
```

Here `peer` is an authenticated, discovered client and `saveIntent` is your storage
callback. Store the intent together with the source event or job that caused it,
so a worker restart finds the original operation instead of creating another one.
The SDK cannot verify your storage durability. Saved intents contain account data;
protect them against disclosure and tampering. They contain no tokens or keys.

For existing subjects, `ReadSubject` returns a validated human record. Carry its
`id`, `revision`, and `authority` into `SubjectVersion`. On a revision conflict,
re-read and make a new source decision explicitly; the SDK does not refresh the
precondition silently. Creation supports a stable source reference, display name
and optional scalar department/email; it does not adopt or link an existing account
by matching email.

`PrepareUpdate(ctx, subjectVersion, ScalarChanges{Set: ..., Clear: ...})` supports
`displayName`, `department` and legacy scalar `email`. Omission preserves a fact;
clear preserves its owner with a null value. The target must implement this scalar
surface; the SDK cannot add it to an older application adapter. These helpers do
not select `human-attributes-v1` or publish verified email assertions.

## Recovery and boundaries

`Intent` keeps request bytes and enrollment identity fixed. Restoring it checks
resource, issuer, tenant, domain, client ID, version and original replay-window
evidence. `Recover` validates the original retention promise as well as the
receipt. It works without current new-work discovery, so a withdrawn catalog does
not erase the ability to inspect an earlier operation.

A failed lookup does not prove non-commit. Expired windows, unavailable results,
authority changes and authentication failures require caller policy; there is no
unbounded retry loop or automatic token renewal. Renew authentication explicitly
and retain the same intent. Client instances are not safe for concurrent use.

The existing CLI's legacy 0.1 diagnostics and test helpers remain available for
compatibility. New provisioning helpers support only single-command human create,
activate, disable and scalar updates. Enumeration retains its separate cursor retry contract.
Structured human attributes, groups, provider adapters, stable API guarantees and full
baseline conformance remain outside this slice.

## Verification

From `sdk/go`, run `go test -race ./...` and `go vet ./...`.
Tests cover saved-intent failures, response loss, recovery with a new client,
byte-identical retries, enrollment mismatch and shortened retention promises.
Existing authentication, receipt, lifecycle and enumeration tests moved with the
implementation. Root-module tests check embedded schema/fixture snapshots against
the canonical repository files.

For a disposable real Memos test, build the pilot with `-tags shrimptest`, then run
from the repository root with the HTTP fixture dependencies installed:

```sh
python pilots/memos/client_sdk.py --memos-binary /absolute/path/to/memos --go /absolute/path/to/go
```

This starts a fresh Memos process and synthetic issuer, creates a disabled account,
activates it, updates and clears scalar facts, and disables it. A new SDK client
then recovers and retries the original attribute update without reactivating it. It
cleans up the disposable deployment and never opens an existing user trial.
The selected checks do not certify a provider integration or production readiness.
