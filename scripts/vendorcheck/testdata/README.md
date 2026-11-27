# Official OpenTofu 1.13.10 scanner fixtures

These are the unmodified govulncheck reports captured on 27 November 2026 for
the authenticated official Linux executables listed in
[the Runner 3.3.1 disposition](../../../docs/releases/vendor-residuals-v3.3.1.md).
The scanner was run using Go `1.26.11`:

```sh
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 -mode=binary -json /path/to/tofu
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 -mode=binary -show=verbose /path/to/tofu
```

JSON exit was `0`; the text invocation exited `1` with scanner `exit status 3`.
The database was `https://vuln.go.dev`, last modified `2026-11-24T15:56:00Z`.
Gzip compression uses modification time `0`; decompressing recovers the exact
original JSON bytes. The text fixture is the AMD64 capture; the ARM64 text
differs only in the `GO-2026-6974` symbol listing.

| Raw report | SHA256 |
| --- | --- |
| AMD64 JSON | `280b2b88ccd276cb969c75a67007c44f50788aee2ba87916eee3e124363a9f2f` |
| ARM64 JSON | `965f70f3369cf99c1e9e34150e625dcaf5bec7b8789b2a183f07d7e4c7cbb1da` |
| AMD64 text | `f816840bb971caa6439a21fdd23577d2547741ec85863ec1d2c478fd3f92da3e` |

Both reports have 14 advisory IDs and 1432 finding records. Canonicalization
removes `module` and `version` from package and symbol trace frames and the
`fixed_version` of those records, keeps module-level records unchanged, uses the
validator's Go JSON encoding of each finding object, sorts those encoded
strings, joins with newlines and adds a final newline. Both produce SHA256
`6b31464e89d4c86c0a36af028c3d4b2ce051652d053127f0bbd7ee3ec84bc765`.
The raw reports differ only in the module labels of package and symbol records:
the scanner labels each package with the first binary module whose path prefixes
it, iterating a Go map, so `GO-2026-6974` packages under nested
`go.opentelemetry.io/otel` modules get a random label. Emission order is ignored; duplicate records, package/symbol identities
and all module-level fields remain significant. The stripped-binary fallback
precision is described in the disposition; record counts do not establish
runtime reachability.
