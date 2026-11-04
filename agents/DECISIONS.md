# Decisions

## 2026-11-04: Stable SDK compatibility notes

List the published CP and DP shared SDK versions separately: CP shared/v3
3.2.0 and DP shared/v3 3.1.0. Keep Runner application code, dependencies,
workflows, recovery limits and the release-scoped OpenTofu residual disclosure
unchanged. Preserve the immutable RC4 source and artifacts.

A normal stable PR creates a new main identity. Bind the stable tag, producer
and original consumer to that actual reviewed identity after landing; do not
substitute the RC source SHA or infer future artifact identities.
