# Official OpenTofu 1.13.2 scanner fixtures

These are the unmodified govulncheck reports captured on 13 November 2026 for
the authenticated official Linux executables listed in
[the Runner 3.3.0 disposition](../../../docs/releases/vendor-residuals-v3.3.0.md).
The scanner was run using Go `1.26.11`:

```sh
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 -mode=binary -json /path/to/tofu
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 -mode=binary -show=verbose /path/to/tofu
```

JSON exit was `0`; the text invocation exited `1` with scanner `exit status 3`.
The database was `https://vuln.go.dev`, last modified `2026-11-11T02:51:11Z`.
Gzip compression uses modification time `0`; decompressing recovers the exact
original JSON bytes. The text reports were byte-identical for both platforms.

| Raw report | SHA256 |
| --- | --- |
| AMD64 JSON | `f0a786c7de0d6148e88e603ab4f050d46f088bd47fa84ddd5aceb2cc87f39c5d` |
| ARM64 JSON | `49b2fb4489aeb0efcef8722056e3cbd208d751067699bbc22e2c7fa70225d162` |
| Text, both platforms | `d7efe3fb5ee9f920b9e4d83deb9fd9141d5558bb80247dc778b31abde1403933` |

Both reports have 13 advisory IDs and 1563 finding records. Canonicalization uses
the validator's Go JSON encoding of each complete finding object, sorts those
encoded strings, joins with newlines and adds a final newline. Both produce
SHA256 `5f8e13d9d42dd50f98ceb36cad83a047487e8847419fa1e22d3a0172eb2525a6`.
Emission order is ignored; duplicate records and all trace/fixed-version fields
remain significant. The stripped-binary fallback precision is described in the
disposition; record counts do not establish runtime reachability.
