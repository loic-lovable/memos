# Disposable SHRIMP pilot

For an interactive trial, the normal `cmd/memos` command now accepts
`--shrimp-config` and serves the full browser app with public SHRIMP routes.
Use `pilots/memos/app_trial.py` and `docs/memos-local-trial.md` in the coordinated
SHRIMP checkout to build and run it with persistent notes and local synthetic SSO.
This separate command remains the disposable fault-test backend.

This command runs the Memos API plus a limited SHRIMP 0.2 integration on loopback
HTTPS. It is separate from `cmd/memos`, has no browser frontend, and supports one
process and a fresh SQLite database. Build with Go 1.27 or newer:

```sh
go build -o /tmp/memos-shrimp-pilot-binary ./cmd/shrimp-pilot
```

Run it through `pilots/memos/run.py` in the coordinated SHRIMP protocol checkout.
That runner generates the private configuration, CA, issuer enrollment, admin
password and test-control secret; starts and stops processes; and records selected
real-application observations. It uses the existing protocol schema catalog rather
than a new wire dialect. The protocol checkout must include commit `b0966b3` and
its pilot runner. This application branch starts at Memos
`7e3d3c63156c206209fdc8e75a4fb117b1aaf1fb`.

## Runtime boundaries

- `server/shrimp` validates the enrolled ES256 issuer, audience, client, DPoP key,
  request method/URL and proof replay. It verifies and serves the exact pinned
  local schema bytes. Discovery declares `profiles: []` and the selected partial
  operation set. Complete-profile requirements are rejected.
- `store/db/sqlite/shrimp.go` commits native account state, source mapping, revision,
  event and operation result in the same immediate transaction. An exact retry
  recovers the immutable result before current support, deadline and revision
  checks. Source IDs cannot be used as subject IDs.
- Native edits to an enrolled user's password or other independently owned fields
  advance the managed revision in their own transaction. Native lifecycle and
  display-name edits are rejected. Native deletion requires prior retirement.
- Password verification reads the credential and revision together. Session,
  refresh and personal-token issuance recheck authoritative account state and
  that original revision before publication. A process-local lock protects the
  write through HTTP flushing, coordinating it with disable and native edits.
  Cached users are not the admission authority. Already-issued access tokens are
  not revoked by this implementation.
- Private controls can hold the real issuance paths and exit the process just
  before/after SQLite commit. These controls are mounted only by this command
  and require a random bearer secret. They do not invent outcomes or receipts.
- A private authentication observation binds one native PAT-creation request to
  its enrolled subject, exact POST route and an opaque one-use handle. The auth
  hook records only when that request's valid access token resolves to an archived
  user; the inspector also requires the actual 401 response. Handles expire after
  60 seconds, reads consume them, and the launcher keeps at most 32. Invalid,
  expired, wrong-user, ambiguous and reused requests cannot establish a denial.
  Inspection requires the separate TLS control credential. Public response status,
  headers and body stay unchanged; neither credentials nor hashes are recorded.
  The ordinary Memos launcher does not install this observer or its controls.

The SQLite database is bound to one resource, issuer/key ID, client and authority.
It cannot be restarted under a different enrollment. The pilot process takes an
OS file lock; arbitrary direct database writers do not participate in that lock.
Do not attach this command to an existing installation or run it alongside another
Memos process using the same database. Backup restoration, key rotation,
organization-backed SSO, public audit/inventory/sync and complete profile support
remain open.

MySQL/PostgreSQL migrations preserve repository schema parity; the pilot facade
refuses to enable those drivers. The normal launcher requires explicit
`--shrimp-config` to enable public SHRIMP routes and refuses an enrolled database
without it. It does not install private fault controls or request observers.

## Validation

```sh
go test -race ./cmd/shrimp-pilot ./store/db/sqlite ./server/shrimp
go test -race ./server/... ./core/...
go test ./store/...
go vet ./cmd/shrimp-pilot ./server/shrimp ./server/api/v1 ./store/...
```

Store integration tests require Docker. With Colima, set `DOCKER_HOST` to its
socket and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`. Released
SQLite image tests also need `TMPDIR` in a directory shared with the VM.

On the development machine, the pre-existing
`TestDetectAttachmentMimeType/unknown_extension_falls_back_to_content_sniffing`
fails both on this branch and on the untouched base: the system MIME database
classifies `.xyz` as `chemical/x-xyz` instead of falling back to PNG sniffing.
The remaining server/core race suite was run with
`-skip '^TestDetectAttachmentMimeType$'`. That exception does not imply the full
unmodified test command passed.
