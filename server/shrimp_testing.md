# SHRIMP fault tests on the Memos server

The interactive local trial and disposable fault suites use the same
`cmd/memos` entry point, server, native APIs, and admission transport. The former
`cmd/shrimp-pilot` launcher has been removed.

For the browser trial, build the frontend with `cd web && pnpm release`, then
build `go build -o /tmp/memos-app ./cmd/memos` from the repository root. Use
`pilots/memos/app_trial.py` and `docs/memos-local-trial.md` in the coordinated
SHRIMP checkout. This regular build does not contain fault controls and rejects
the `test` configuration field.

For disposable fault tests, explicitly include the test code:

```sh
go build -tags shrimptest -o /tmp/memos-shrimp-test ./cmd/memos
```

Run it through `pilots/memos/run.py` or the shared-client harnesses in the
coordinated SHRIMP checkout. They use the normal `--addr`, `--port`, `--data`,
`--instance-url`, `--trusted-proxies`, and `--shrimp-config` options. Frontend
assets are optional for API-only fault runs.

The harness supplies a private runtime file with the ordinary `shrimp`,
`certificate`, and `key` fields, plus a `test` object containing distinct random
`control_token` and `audit_token` values. The tagged build still requires a
non-demo SQLite instance on loopback TLS. Without that explicit object, the
tagged build mounts no private controls. Never use a test build for real data.

The harness creates the first administrator through the native API. The normal
application creates and persists its session-signing secret. Tests do not supply
an alternative secret or bypass normal server initialization.

## Preserved test controls

`/__pilot/control` can hold distinct native credential-publication requests or
terminate the process immediately before or after SQLite commit. It requires
the private control credential and is compiled only with `shrimptest`.

The same control route can arm and inspect a bounded, one-use observation of a
particular native PAT-creation request. The observation requires the enrolled
subject, exact route, valid access-token authentication refusal, and actual 401
response. Invalid, expired, ambiguous, reused, and wrong-user requests cannot
establish a denial. Public responses and credentials are not recorded or changed.

`/__pilot/audit` requires the separate audit credential. It exposes the existing
bounded, redacted operational journal. These private controls are test evidence,
not portable protocol features or a complete conformance claim.

Admission protection remains held through the real response flush. Its regression
now lives in `server/shrimp_transport_test.go` and runs in both build modes.

## Validation

```sh
go test -race ./server ./server/shrimp ./cmd/memos
go test -race -tags shrimptest ./server ./server/shrimp ./cmd/memos
go vet ./server/... ./cmd/memos
go vet -tags shrimptest ./server/... ./cmd/memos
```

Also run the dedicated and shared SHRIMP harnesses after changing the controls
or server wiring. Store integration tests require Docker; with Colima, set
`DOCKER_HOST` to its socket and
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`. Released SQLite
image tests need `TMPDIR` in a directory shared with that VM.

The pre-existing
`TestDetectAttachmentMimeType/unknown_extension_falls_back_to_content_sniffing`
fails on the development host's MIME database, which classifies `.xyz` as
`chemical/x-xyz`. Any run excluding it must report the exclusion explicitly.
