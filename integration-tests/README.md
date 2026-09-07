# Runner integration fixtures

The Module Version Management release must be tested with a compatible Control
Plane, Data Plane and IAM image set. Old pre-versioning images are not valid
substitutes. CI requires repository variables `CORE_CP_INTEGRATION_IMAGE`,
`CORE_DP_INTEGRATION_IMAGE` and `CORE_IAM_INTEGRATION_IMAGE`, each pinned by
`@sha256:` digest. Their exact source revisions must be recorded together in the
release manifest before publication. Local Make overrides (`CP_IMAGE`,
`DP_IMAGE`, `IAM_IMAGE`) accept locally built candidate tags, without a push.

`make test-integration` builds the Runner from the current worktree. Test Module
fixtures explicitly publish SemVer versions and promote their initial Default;
the retained generated CP fixture client is adapted at its request boundary,
not by relaxing server lifecycle validation.

## Optional isolated provider mirror

Provider download latency can exceed the existing deployment assertions even
when Runner execution is healthy. The test-only image below adds an OpenTofu
filesystem provider mirror to the **same Runner binary**. It changes neither the
production image nor the test deadlines. The fixture contains only versions of
providers already required by these integration tests. Do not publish this image
as a production Runner.

```sh
docker build -t platform-orchestrator-runner:local .
docker build -f integration-tests/provider-mirror/Dockerfile \
  --build-arg RUNNER_IMAGE=platform-orchestrator-runner:local \
  -t platform-orchestrator-runner:test-cached .
kind load docker-image --name runner platform-orchestrator-runner:test-cached
```

Set `RUNNER_TEST_IMAGE=platform-orchestrator-runner:test-cached` for the Go
integration invocation. Direct Runner fixtures and remote-agent fixtures use
that image; deliberately invalid images in failure tests remain untouched.
`RUNNER_TEST_GATEWAY_URL` may supply the gateway address reachable from the
explicitly isolated test cluster. Neither override should target a shared or
production cluster. The dedicated gateway E2E fixture uses its own explicit
`PO_GATEWAY_KIND_*` settings instead.

The normal registry installer builds the mirror; no plaintext credentials or
custom provider installation implementation is included. Missing provider
versions are not silently taken from this mirror. Refresh its explicit fixture
versions when the corresponding integration fixture constraints change.

## Legacy artifact contract

Run `IAC_TEST_BINARIES=tofu,terraform make test-artifact-integration` from the
repository root, or `docker build --target artifact-integration .` to exercise
the OpenTofu binary shipped by the standard Runner image.

The test applies a real local Git module through pre-management deployment,
migrated v0 carry-forward, first managed version, and exact history rollback.
Only authoritative `migration_generation: v0` permits a missing historical
external-artifact claim. Managed or unidentified versions still require their
claim; all external sources must actually have been downloaded. Artifact
availability checks do not claim trusted digest verification, which is deferred
in this Core iteration.
