# Stellwerk Platform Orchestrator Runner

This is the image executed in customers infra to execute [OpenTofu](https://opentofu.org/).

It is responsible of:

1. Fetching the Terraform File from `platform-orchestrator-dp`.
2. Running `tofu apply`.
3. Inspecting the results.
4. Calling the API back.

## Publishing Helm Chart

The Helm chart for this runner is located in the `charts/platform-orchestrator-kubernetes-agent-runner` directory.
To publish a new version of the chart, update the version in `Chart.yaml` and push the changes.
A GitHub Actions workflow packages and publishes the chart to
`ghcr.io/stellwerk-labs/charts/platform-orchestrator-kubernetes-agent-runner`.

Publishing behavior:

- **Push to `main`**: Publishes a stable version (no suffix)
- **Push to other branches**: Only publishes if the commit message contains `[publish]`, and the version will have a `-beta` suffix
- **Manual workflow dispatch**: User can choose via checkbox whether to add the `-beta` suffix to the version
