# Experimental Go SDKs

Choose the [provisioning client](client/README.md) to send account changes from a
provider or bridge. The server SDK below helps an application receive them.
Both APIs remain experimental; neither includes a database or a provider bridge.

The [profile boundary design](../../docs/design/sdk-profiles.md) describes the
split between transport, reusable profile rules and application transactions.


[Documentation](../../docs/README.md) · [Memos integration](#memos-integration) · [Database boundary](#database-boundary) · [Extraction evidence](#extraction-evidence)

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

`Apply` returns stable sentinels for known failures: `ErrReplayConflict`,
`ErrOperationResultUnavailable`, `ErrUnsupportedProfile`, `ErrInsufficientScope`
and `ErrExecutionDeadlineExpired`. `Read` uses `ErrInvalidDependency` for missing
causal evidence and `ErrNotFound` for an absent record. Wrap sentinels with `%w`
to add internal context; the handler uses `errors.Is`. A missing operation result
still has an unknown commit outcome. Return an unclassified error when storage
cannot establish what happened; it becomes a bounded storage-unavailable response.
Retained failed operation outcomes are carried in `Result.Error`; they are
serialized protocol codes, not Go errors, and their stored representation is unchanged.

The original exact string codes remain accepted by a deprecated compatibility
fallback while existing adapters migrate. That fallback does not recognize
substrings or wrapped legacy strings. New adapters must use the sentinels. For
example, use `fmt.Errorf("read causal evidence: %w", server.ErrInvalidDependency)`
rather than constructing an error whose text happens to be `invalid_dependency`.
Enumeration uses `EnumerationError` for the documented public cursor and selection
failures. Private error text is never returned to clients.

The bounded operation set is discovery/schema access, replay windows, one human
account command per mutation, retained operation reads, direct reads and account
enumeration. By default, creation requires a source reference and display name;
updates change only display name. The opt-in owned scalar extension below permits
additional scalar fields and explicit clears. Mutation commands are create, activate, disable, retire and
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

## Memos integration

The package owns HTTP routing, strict message validation, trusted API authentication, discovery, and wire encoding. Tenant, domain, history epoch, discovery revision, and admission consumer are explicit application settings.

Memos implements `Application` in `server/shrimp/adapter.go`. Its `Store.ApplyShrimp` operation commits the native account, source association, revisions, retained result, and required journal records together. The same lock coordinates mutations with native writes and credential publication. A later `onDisable()` callback cannot establish that the account and required effects changed with the result.

Memos keeps its native account and SSO mapping, password/session behavior, schema and migrations, and protection through HTTP response flush. Fault controls and certificate tooling remain outside the SDK. The public Memos fork consumes an unchanged SDK snapshot, with its license and revision in `third_party/shrimp-sdk/SOURCE.md`; its relative module replacement needs no access to the internal SHRIMP repository.

See the [pilot architecture](../../pilots/memos/architecture.md), [browser trial](../../pilots/memos/local-trial.md), and [adoption plan](../../docs/design/adoption-tooling.md).

## Database boundary

The core SDK owns no database connection, schema, migrations, or transactions.
It takes application operations through an interface, with no SQL driver, ORM,
or database transaction types in that interface. The application chooses how to
store data and implements the required durability and atomicity guarantees.

For example, the SDK validates a disable request and calls the application's
atomic operation. Memos commits the account change and its required result and
journal records together, then returns the outcome for the SDK to encode.
If the response is lost, a retry retrieves that retained result. The SDK must
not commit a separate result store before or after an account-change callback:
a crash between those commits could leave the account and result inconsistent.

Durable result lookup and proof replay protection still require storage, supplied
by the application through the adapter. Keeping database access outside the SDK
does not make these protocol requirements optional. The first SDK extraction
includes no bundled persistence implementation.

## Verification and next adoption step

1. Keep storage and admission checks in the application. Verify that the core SDK
   interface exposes no database handles or transaction types and that its package
   contains no database setup or migrations.
2. Test the separate module with `go test -race ./...` and `go vet ./...` from
   `sdk/go`. The SDK workflow runs these checks independently of the CLI module.
3. Run the same dedicated, shared read/lifecycle/enumeration and SSO checks.
   Preserve account identity, exact retry recovery, old-login refusal after
   disable/restore, note preservation, and all negative/inconclusive controls.
4. Use a second Go application to find assumptions specific to Memos before
   stabilizing the API. Do not equate two apps using one SDK with independent
   protocol implementations.

The [client SDK](client/README.md) is now extracted from the existing Go CLI.
Additional languages, a provider bridge, multiple database processes, and complete
profile claims are outside this first server SDK slice.

## Extraction evidence

Memos commit `d8c2c65e` consumes SDK source from SHRIMP commit `a9a4d4e`.
Clean-build shared checks pass: seven read cases, 27 lifecycle cases, and five
account-listing cases. The expected negative and inconclusive controls remain.
The SDK-backed normal browser application also passes the four API observation
groups covering local SSO, exact account binding, disable across restart and note
preservation. The 22 dedicated fault/recovery groups pass on the development build.

SDK race tests and vet, regular and tagged Memos adapter/server tests, 14 Python
harness tests and repository checks pass. The broader server/core regression run
excludes the previously reproduced host MIME mismatch in
`TestDetectAttachmentMimeType`. These checks are selected real-application and
unit-test evidence, not a complete profile or production-readiness claim.


## Owned scalar pilot extension

Applications may opt into `Config.ScalarAttributes` only when their `Apply`
implementation atomically validates and persists `Mutation.Set`, `Mutation.Clear`
and per-field authority/revision, and returns `Subject.Attributes` on reads and
enumeration. This adds exact `displayName`, `department` and scalar `email` in the
existing representation. Clears are owned null facts; unrelated lifecycle changes
must preserve field revisions. Memos supplies this storage and maps display name to
its native nickname. Other application adapters retain the display-name-only default.

This scalar extension does not select or advertise `human-attributes-v1`; the
separate typed development switch is described below. The [Memos mapping](../../pilots/memos/human-attributes.md)
records that profile's remaining implementation work.

`profiles/scalar` supplies the shared compatibility types, decoding and pure
owned-fact transition. `server.ScalarFact` and `client.ScalarChanges` remain
compatible aliases. Applications can call `scalar.Apply` against current facts
inside their own transaction, then commit its result with native changes and
retained evidence. Resolve legacy ownership explicitly before calling it; the
helper cannot infer an owner. It does not check the subject revision, authorize
the caller, manage a replay window or commit anything.

## Typed human attribute helpers

[`profiles/humanattributes`](profiles/humanattributes/README.md) provides strict
attribute-fragment decoding, typed values, validation and proposed transitions for
structured names, email entries/generations and locale/timezone preferences. It checks field ownership,
preserves exact facts, refuses retired entry keys and returns the email versions
whose verification assertions the application must invalidate atomically.
Callers supply trusted profile configuration and durable identifier history.
Errors support `errors.Is` categories and `errors.As` field details.
`DecodeValues` and `DecodeChanges` reject ambiguous JSON, missing members and
unknown fields under explicit byte/depth bounds. The transport must still validate
the whole envelope and resolve retained operations before current new-write rules.

The package also proposes explicit scalar migration and validates stored state
independently of current write limits. Transport dispatch and Memos transactions
are connected through the development boundary below. Importing the helper does
not activate `human-attributes-v1`; discovery remains `profiles: []`. Helper tests
and real-application observations remain separate evidence.

## Typed human attribute integration tests

`Config.ExperimentalHumanAttributes` dispatches selected typed create/update and
explicit `migrate_human_attributes` commands to an application's transaction.
This is a development switch: discovery still advertises no profiles, and
requests requiring `human-attributes-v1` still fail closed. It is not a supported
profile deployment or a substitute for its baseline dependencies.

The handler strictly checks the whole JSON envelope, including duplicate members
and malformed Unicode, before computing retry equality. `Mutation.HumanAttributes`
contains the exact parsed values/changes fragment; the application uses its
configured `humanattributes.Profile` to decode it only after retained lookup and
intent equality. This preserves recovery when timezone catalogs, email limits,
write permission or profile support change. `Subject.HumanAttributes` contains
public facts only; identifier history stays in application storage.

`Mutation.Migration` carries an opaque administrative handle and an approval
fingerprint. The latter is SHA-256 of canonical complete request JSON with the
command's `authorization` member omitted. The retry fingerprint includes the
handle. The application checks the grant against the exact principal, operation,
scope and affected authorities, then atomically consumes it with representation,
state, old-writer fence and retained result. Supplying a handle does not authorize
anything on its own. The SDK contains no approval database or administrator role.

The Memos [typed development runner](../../pilots/memos/human_attributes.py)
checks this boundary against the real app. Normal Memos builds refuse the switch;
only the disposable `shrimptest` build enables it. Atomic attribute-plus-lifecycle
commands and broader profile obligations remain before advertisement.
