# SHRIMP SDK source snapshot

Copied from `sdk/go` in `github.com/lovablelabs/shrimp-protocol` at
`0bb4b2fb7bdad6fc39d743d91ea7a2c54201a4ca`. The upstream repository is internal; this checked-in copy lets
public Memos builds use the experimental SDK without private repository access.

All other files in this directory are unchanged from that revision, including
its module definition, tests and MIT license. Changes belong upstream first;
refresh this directory and record the new revision when updating the SDK.
Memos storage and login integration remain in `server/shrimp` and `store`.

From this directory run `go test -race ./...` and `go vet ./...`.
