# Retained plans with external providers

> **RC feature:** This page describes Deployment Pipelines in the coordinated
> release candidate (Runner `3.4.0-rc.1`). Pipelines are not available in the
> current stable release from `main`. Use matching Pipeline RC components; Native
> execution requires explicit operator activation.

Saved plans retain the lockfile and exact provider identity required to apply the
reviewed result. Provider packages remain in the site's verified cache or mirror;
the encrypted plan archive stores their manifest instead of bundling binaries.
The ordinary legacy Runner retained-plan behavior is unchanged.

- Before Plan, installed provider packages are materialized into private work-directory regular files. Their exact file hashes and executable modes are recorded. Only recognized provider package cache links are dereferenced; symlinked parent directories and unsupported file types fail closed.
- The retained encrypted snapshot includes the lockfile and exact provider-file manifest, while omitting `.terraform/providers` binaries. Existing archive, expanded-size, tool-hash, binding, OS and architecture checks remain. Provider materialization is bounded to 64 packages, 4096 files and 1 GiB total copied bytes.
- Provider-aware snapshots use binding schema `stellwerk.retained-plan/v2` in the unbound profile; Native PostgreSQL uses UUID-bound `stellwerk.retained-plan/v3`. Older Runners reject unsupported bindings before Apply. Existing `v1` snapshots remain supported in their original profile; a provider manifest cannot be hidden in a `v1` snapshot. Newly produced snapshots require provider identity captured by the actual Plan path.
- Restore runs `init -backend=false -get=false -lockfile=readonly`, verifies the selected packages against the retained lockfile, materializes private copies, and requires their complete manifest to match the reviewed identity. It does not generate a new Plan or initialize/migrate state storage. A mirror changed before restore fails; a mirror changed after restore cannot alter the private provider executed by Apply.
- Development overrides, provider reattachment and checksum-bypass cache settings are unsupported for retained plans. Bounded CLI-configuration inspection rejects them before Plan and on restore, including JSON/HCL quoted escaped override keys and case-folded checksum-bypass fields. Selection follows actual supported tool precedence (`TF_CLI_CONFIG_FILE`, then deprecated `TERRAFORM_CONFIG`); `TOFU_CLI_CONFIG_FILE` does not suppress active defaults. Main files, default `.tfrc`/`.tfrc.json` fragments and OpenTofu 1.13 XDG fallbacks are inspected. Inspection is deliberately conservative, so mentioning unsupported settings in comments is also refused. Native configuration files must remain operator-controlled during a run.

For Native reviewed execution, restore and provider verification finish before the durable Apply effect claim. Fresh restore refusal is a known failure with no Apply claim or infrastructure effect; uncertainty after a claim or process start keeps the existing unknown-outcome fence.

Existing absolute configuration/module/data-path portability limits are unchanged. Use the same OS, architecture and tool identity; arbitrary externally resolved module paths are not portable. Provider installation must remain available through the site's configured verified cache/mirror. An air-gapped site needs those packages locally, without requiring a registry connection during offline execution.

References: [Terraform automation](https://docs.hashicorp.com/terraform/tutorials/automation/automate-terraform), [Terraform init](https://developer.hashicorp.com/terraform/cli/commands/init), [OpenTofu init](https://opentofu.org/docs/cli/commands/init/), [Terraform development overrides](https://docs.hashicorp.com/terraform/cli/config/config-file).
