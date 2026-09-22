# Typed human attribute helpers

**Status:** experimental decoding, validation and transition helpers for
[`human-attributes-v1`](../../../../spec/human-attributes.md). The SDK HTTP handler
and Memos do not enable this profile. These helpers alone are not a complete
profile implementation or real-application conformance evidence.

Import `github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes`.
The package handles `displayName`, structured `name`, `department`, `emails`,
`locale` and `timezone`. It preserves supplied spelling and distinguishes omitted,
empty and explicitly cleared facts. Each fact keeps its owner and revision.
The package opens no database, changes none of its inputs and commits nothing.

## Configure a validator

Construct a `Profile` with `New(Config{...})`. Supply `MaxEmails` (1–16), a pinned
IANA `TZDBVersion`, and every Zone and Link name from that upstream release,
including backward aliases. Obtain and verify the release artifact in application
setup. `New` copies and validates the catalog's shape; it cannot establish that
your list is complete or comes from that release. It never consults host settings.

For example, `US/Eastern` is preserved as that spelling when the pinned release
contains it. A later catalog upgrade does not rewrite existing facts. A new write
must use a name in the currently configured catalog. Locale checks enforce the
profile's language-tag syntax and reject repeated variants/extension keys while
preserving case, such as `EN-us`.

The small catalogs in unit tests are fixtures, not complete release catalogs.
This increment includes no release loader or bundled timezone database.

## Decode attribute fragments

Use `Profile.DecodeValues(raw)` for an `attributes` or `set` object, and
`Profile.DecodeChanges(raw)` for exactly `{"set":{...},"clear":[...]}`. The latter
requires both members and at least one change. The decoders enforce exact member
names, closed object shapes, required email members and configured validation.
They preserve an empty email array separately from omission and reject null field
values; explicit clearing belongs in `clear`. A new email entry must include
`"expected_generation": null` and a boolean `primary`, even when it is false.

Duplicate members (including escaped spellings), malformed Unicode, trailing
documents, set/clear overlap and repeated clear fields are rejected. Each fragment
is limited to `MaxJSONBytes` (65,536 bytes) and `MaxJSONDepth` (16 nested containers,
counting the root as one). Syntax errors use `ErrInvalidValue`; exceeded byte or
depth bounds use `ErrLimitExceeded`. Every failure returns a zero result and leaves
the input bytes unchanged. Errors never include the supplied JSON or field values.

These methods are not complete request decoders. The transport must strictly
parse and validate the entire envelope, including its own byte/depth limits,
profile selector, command shape and unknown members. Do not first unmarshal into
a permissive map and then re-encode a fragment: information about duplicates or
invalid Unicode has already been lost. A validated raw fragment can be passed
without that lossy conversion.

The configured decoders enforce **current new-write rules**. Resolve authorized
retained operations and compare original intent before applying those rules.
Recovery after a limit/catalog change must use the original accepted contract;
it must not reject retained work merely because today's decoder would refuse a
new write. Successful decoding also says nothing about current field ownership,
subject revisions or email generations. Check those in the application transaction.

## Calculate a change inside the application's transaction

1. Authenticate and authorize the writer, recover retained operations before new
   work, and validate the subject's identity, representation, lifecycle and
   expected revision against current stored state.
2. Pass current `State`, typed `Changes`, the trusted authority and a fresh
   revision to `Profile.Update`. For a new selected-profile human, use `Create`.
   Creation is not a way to adopt or migrate a legacy record.
3. Commit the returned facts, identifier history, native projections and all
   `Invalidated` effects atomically with the subject revision and retained
   operation result. An error returns no partial result. On a rejected commit,
   discard the entire proposed result.

`Validate` checks already typed write values only. Use the fragment decoders above
for JSON values. Ordinary `json.Unmarshal` into these structs cannot enforce
required members or reject every unknown/duplicate member. These types are field
values, not authenticated mutation envelopes.

An email entry has a caller-assigned key and a target-assigned generation that
identifies that particular address value. Existing entries require their current
`ExpectedGeneration`. Changing an address allocates a fresh generation; changing
only type, primary preference or order preserves it. Removing an entry prevents
its key from being reused. `Invalidated` identifies each removed/replaced value
whose verification assertions must change in the same application transaction.
The helper neither creates verification evidence nor performs that invalidation.

Supply a `GenerationAllocator` when creating or changing an address. It must
return a fresh, target-controlled token and publish no external effects. A failed
transition can leave unused allocations; it never consumes identifiers in the
input state. Allocation failure, malformed output and generation reuse all fail
the complete transition.

`State.EntryIDs` and `State.Generations` contain all committed identifiers for
that subject, including removed entries and superseded values. Store this history
durably with the facts. Reads of current facts cannot reconstruct retired keys.
History grows with accepted changes; this helper materializes it and has no
compaction or eviction policy. Applications must account for its storage and
transaction cost. Do not discard history while the subject can accept writes.
The Go state shape does not prescribe a database layout.

After strictly decoding persisted state, call `ValidateState` before returning
facts or committing a lifecycle-only change. It rejects inconsistent ownership,
revisions and email identifier history without applying today's email limit or
timezone catalog to historical facts. Empty state is valid. It does not reject
unknown or duplicate JSON members, authorize an operation, or prove that omitted
history never existed; those checks remain with the storage adapter.

## Calculate an authorized legacy migration

`Profile.Migrate(current LegacyState, emailEntryID *string, revision string,
allocate GenerationAllocator)` prepares the attribute result of
`migrate_human_attributes`. Call it in the application transaction after recovering
retained work, checking that the subject is a non-retired human still using the
compatibility representation, comparing its whole-subject revision, and validating
the exact administrative migration approval and each affected field's approval.
The helper accepts no caller authority and cannot establish those permissions.

`LegacyState.Facts` contains only the compatibility `scalar.Fact` values for
`displayName`, `department` and `email`. Display name and department retain their
exact values, owners and revisions. A valid non-null email becomes one primary
entry with no inferred type, preserving its value and owner. Supply its never-used
entry key and a generation allocator; the `emails` fact gets the new revision,
which cannot equal the old email revision. An empty or unrepresentable legacy
email rejects the entire migration without trimming or rewriting it.

An absent email remains absent. An explicitly cleared email becomes an owned-null
`emails` fact with its original owner and the new revision. Both cases require a
nil entry key and allocate no generation. No other rich fields or verification
assertions are inferred. The returned `Invalidated` list is empty.

Include all retained `EntryIDs` and `Generations` in `LegacyState`, including
history carried through restoration. Nil histories are valid only for genuine
first use; the helper cannot recover omitted history. It copies retained history
and rejects entry or generation reuse. Invalid metadata/history fails with
`ErrInvalidState`; an unrepresentable email or wrong entry-key selection uses
`ErrInvalidValue`. Generation failures use the same categories as ordinary
transitions. Every failure returns a zero `Result` and leaves input state intact.

Commit the proposed facts, histories, representation fence, new subject revision,
native projections and retained operation result together. Authorized old scalar
operations must retain their original recovery meaning, while new scalar writes
must fail after migration. The pure helper provides neither that transaction nor
the transport, authorization or persistence integration.

## Handle errors by category

Use `errors.Is` for control flow and `errors.As` for optional field information.
Error text is diagnostic and may change. For example:

```go
if errors.Is(err, humanattributes.ErrRevisionConflict) {
    // Re-read current state before preparing a new operation.
}
var detail *humanattributes.Error
if errors.As(err, &detail) {
    // detail.Field is a known field, or empty for a general error.
}
```

| Category | Meaning |
|---|---|
| `ErrInvalidValue` | Invalid field shape, clear selection or collection invariant |
| `ErrLimitExceeded` | Email collection or JSON fragment exceeds its bound |
| `ErrAuthorityConflict` | Changed fact belongs to another authority |
| `ErrRevisionConflict` | Stale email generation or unavailable entry key |
| `ErrInvalidConfiguration` | Invalid catalog, missing allocator/authority/revision or reused output revision |
| `ErrInvalidState` | Stored facts or identifier history are inconsistent |
| `ErrGenerationAllocation` | Allocator failed or returned an invalid/reused generation |

The application maps categories to public outcomes only after its authorization
and recovery checks. Do not treat internal state/configuration failures as bad
user input, or automatically retry a conflict as a new operation. Subject-level
revision checks remain the application's responsibility.

Error messages omit values, supplied identifiers and private allocator details.
The allocator's original error remains in the unwrap chain for internal
`errors.Is`/`errors.As` handling; never serialize that chain to a protocol client.

## Evidence and remaining integration

Package tests exercise exact values, ownership, absent/empty/clear semantics,
locale/mailbox validation, pinned catalog membership, generation transitions,
retired key preservation after state serialization, and rejection without partial
changes. They also check error classification through wrapping and allocator
failure after a partial calculation. Decoder tests cover malformed/ambiguous JSON,
Unicode, exact byte/container-depth boundaries and representative schema agreement.
Fuzz tests check rejection without partial values and successful round trips.
Migration tests cover mixed owners, preserved revisions and spelling, absent/null
mapping, retained fences after serialization, independent output copies, malformed
legacy data and allocator failure without partial results.
Run `go test -race ./profiles/humanattributes` and `go vet ./...` from `sdk/go`.

Complete envelope decoding, migration authorization/commit integration, transport
dispatch, profile activation, application persistence/enforcement and complete
observation surfaces remain integration work. The current handler still advertises
`profiles: []`. See the [SDK boundary design](../../../../docs/design/sdk-profiles.md).
