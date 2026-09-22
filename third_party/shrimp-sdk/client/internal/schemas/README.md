# Pinned client schemas

These JSON files are a byte-for-byte snapshot of the repository's `schemas/`
directory. The root Go test `TestSDKContractSnapshotMatchesSource` detects drift.
Copy updated schema files here when changing the canonical contracts. The client
loads only these local schemas; it never follows remote schema references.
