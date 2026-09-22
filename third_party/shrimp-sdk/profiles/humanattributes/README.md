# Typed human attribute helpers

**Status:** experimental validation and transition helpers for
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

`Validate` checks typed write values only. Raw JSON must first pass strict parsing
and the selected profile schema. Ordinary `json.Unmarshal` into these structs
cannot enforce required members or reject every unknown/duplicate member. These
types are field values, not authenticated mutation envelopes.

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
| `ErrLimitExceeded` | Email collection exceeds the configured bound |
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
failure after a partial calculation. Run `go test -race ./profiles/humanattributes`
and `go vet ./...` from `sdk/go`.

Strict wire decoding, authorized conditional migration, transport dispatch,
profile activation, application persistence/enforcement and complete observation
surfaces remain integration work. The current handler still advertises
`profiles: []`. See the [SDK boundary design](../../../../docs/design/sdk-profiles.md).
