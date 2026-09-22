# Persist a provisioning decision and recover it

This runnable example uses the experimental [Go client](../../client/README.md)
against an explicitly enrolled, disposable application. It creates a disabled
human, updates owned scalar attributes, activates and disables the account, and
recovers the original operation after a new process starts.

Run from `sdk/go`. You need Go 1.27, Linux or macOS, `jq` for the shell examples,
and an existing [enrolled client profile](../../../../docs/cli.md) with private
keys and verified HTTPS trust. For the partial Memos pilot, the enrollment must
explicitly select `"required_profiles": []`; the example never changes discovery
requirements for you. Use synthetic account data and a disposable target.

Build the command and select an existing private storage directory owned by your
local user. Its creation must already be durable; the example syncs each new job
entry, intent file and job directory. A filesystem that cannot honor those syncs
cannot provide the required persistence guarantee. This is a local single-user
example, not a multi-user job service or proof of power-loss durability.

```sh
go build -o /tmp/shrimp-provision ./examples/provision
profile=/absolute/path/to/client.json
jobs=/absolute/path/to/private-provisioning-jobs
```

The `jobs` directory must already exist with mode `0700`. Each action below names
a new child directory, so rerunning a mutation command cannot silently replace
an earlier operation. Continue only after an `outcome` of `succeeded` and exit
status zero. Pending, failed, superseded and unknown outcomes exit nonzero.

Create a disabled account with a stable source reference:

```sh
/tmp/shrimp-provision -profile "$profile" -job "$jobs/create" -action create \
  -authority hr-authority -source sdk-example-42 -display-name 'Example account' \
  > "$jobs/create-result.json"
subject=$(jq -r '.resources[] | select(.resource.type == "subject") | .resource.id' "$jobs/create-result.json")
revision=$(jq -r '.resources[] | select(.resource.type == "subject") | .revision' "$jobs/create-result.json")
```

Update the account using that exact observed revision:

```sh
/tmp/shrimp-provision -profile "$profile" -job "$jobs/update" -action update \
  -authority hr-authority -subject "$subject" -revision "$revision" \
  -department Research > "$jobs/update-result.json"
revision=$(jq -r '.resources[] | select(.resource.type == "subject") | .revision' "$jobs/update-result.json")
```

Activate it explicitly, then make a separate disable decision at the returned
revision. These are intentionally separate operations, not a combined atomic
attribute/lifecycle request:

```sh
/tmp/shrimp-provision -profile "$profile" -job "$jobs/activate" -action activate \
  -authority hr-authority -subject "$subject" -revision "$revision" \
  > "$jobs/activate-result.json"
revision=$(jq -r '.resources[] | select(.resource.type == "subject") | .revision' "$jobs/activate-result.json")
/tmp/shrimp-provision -profile "$profile" -job "$jobs/disable" -action disable \
  -authority hr-authority -subject "$subject" -revision "$revision" \
  > "$jobs/disable-result.json"
```

Recover the update from a new process after disable. This returns the update's
historical receipt and does not reactivate or change the current account:

```sh
/tmp/shrimp-provision -profile "$profile" -job "$jobs/update" -action recover
/tmp/shrimp-provision -profile "$profile" -action read -subject "$subject"
```

If a reply was lost, preserve its job and use `recover` first. An unavailable
lookup does not establish non-commit. When an exact resubmission is appropriate,
`-action retry -job "$jobs/update"` restores and resends only the saved request;
it accepts no replacement attributes, revision or deadline. Both recovery and
retry work without new-work discovery. Recovery requests read authority; retry
requests write authority. Authentication cannot change the saved enrollment.

The example never automatically polls, retries, refreshes a subject revision, or
chooses a replacement operation. Missing, partial, altered or foreign-enrollment
intents stop the run. An interrupted setup can leave an incomplete job directory;
inspect it before deciding what new work is safe. Never delete uncertain work to
make a fresh create succeed.

`intent.json` contains account data. Protect it from disclosure and modification,
including other processes running as your user. The sample saver refuses symlink,
non-regular, foreign-owned or publicly accessible intent files and conflicting
replacement bytes. Storage sync failure prevents submission. Keep the saved intent
until your retention and recovery policy permits removal.

Run `go test -race ./examples/provision` for persistence, refusal and outcome tests.
The disposable Memos run also exercised this executable's create, update,
activation, disable, historical recovery, exact retry, unchanged current disabled
state, and read-only recovery after target restart. Those eight observations are
real application evidence for this bounded scalar workflow, separate from local
storage tests and complete profile conformance.
