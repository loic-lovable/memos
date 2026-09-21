# Experimental Go server SDK

This module handles a bounded SHRIMP 0.2 HTTP surface extracted from the Memos
pilot. Its API is experimental. It advertises `profiles: []` and does not implement
a complete conformance profile. A second application has not validated the API.

Import `github.com/lovablelabs/shrimp-protocol/sdk/go/server`. Implement
`server.Application`, provide trusted `server.Config`, then mount the handler
returned by `server.New` on an HTTPS server. Supply the pinned 0.2 schema files
through `SchemaDirectory`; the SDK resolves references only within that catalog.
Enrollment, issuer key provisioning and transport setup belong to the application.

The package owns no database connection, schema, migrations or transactions. Its
application interface exposes protocol data and context only. An application must:

- Commit the account change, retained result and required journal records in one
  atomic operation, coordinating with native writes and credential publication.
- Return immutable retained results on retries and enforce deadlines, conditional
  revisions, dependencies, recovery-only requests and unsupported-profile refusal.
- Store proof replay protection durably and enforce replay-window quotas, lifetime
  and result retention. Translate missing results to `ErrNotFound`, replayed proofs
  to `ErrProofReplay`, exhausted window quotas to `ErrWindowQuota`, and retained
  operation capacity to `ErrCapacity` (new work receives a throttled response).
- Preserve the native audit attempt in the context returned by `BeginAudit`, use it
  during `Apply`, and finish journal publication before a response is released.
- Enforce enumeration cursor binding, retained continuation state and quota limits.
  Retried cursors observe current records from the same position without extending
  their lease; a changed page replaces its previous successor branch. Every
  candidate passed to the size callback must be a prefix of one fixed observation.

`Apply` retains the extracted application's public failure-code contract:
`replay_conflict`, `operation_result_unavailable`, `unsupported_profile`,
`insufficient_scope`, and `execution_deadline_expired` are exact error strings.
Other errors become a bounded storage-unavailable response. Retained failed
operation outcomes are carried in `Result.Error`; they are not transport errors.
Enumeration uses `EnumerationError` for the documented public cursor and selection
failures. Private error text is never returned to clients.

The bounded operation set is discovery/schema access, replay windows, one human
account command per mutation, retained operation reads, direct reads and account
enumeration. Creation requires a source reference and display name; updates change
only display name. Mutation commands are create, activate, disable, retire and
update. SSO correlation, native account identifiers, login/session behavior and
credential publication through HTTP response flush stay in application code.
The SDK includes no fault controls or operational audit inspection endpoint.

The first extraction intentionally fixes the pilot's limits: 64 KiB requests,
1 MiB enumeration pages, JSON depth 16, one command, 16 dependencies, 100 records
per page, 128 open replay windows and enumerations per principal, 300-second
windows/cursors and at least 24-hour results/causal tokens. Other declarations
remain in `server/discovery.json`. The application must meet those declarations;
`HealthyConditions` must describe its actual operating limits. `AdmissionConsumer`
is the application's name for enforcement it has established, not enforcement
provided by this SDK. The SDK neither revokes existing sessions nor verifies the
adapter's persistence guarantees.

A successful disable or retire result means admission blocking was complete by
`Result.Time`. The adapter must retain that evidence with the immutable result;
asynchronous blocking is unsupported. `Read` resolves both subject and source
reference IDs, so those namespaces must have unambiguous IDs.

From this directory, run `go test -race ./...` and `go vet ./...`.

The extracted source retains the Memos MIT notice in [LICENSE](LICENSE).
