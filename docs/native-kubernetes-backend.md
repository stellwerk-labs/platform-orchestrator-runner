# Native Kubernetes state backend

Native PostgreSQL workers use an explicit, self-contained Kubernetes configuration for the state backend. Set the existing private `kubernetes_backend_config_path` to a regular file, at most `1 MiB`, with no world permissions. Configure the registered Runner's ordinary Kubernetes namespace separately. The local configuration selects cluster access; it does not replace the accepted namespace/state key in the compiled deployment.

The initial supported profile requires an explicit current context, an absolute HTTPS server and nonempty valid inline `certificate-authority-data`. TLS verification is mandatory. The endpoint may have a reverse-proxy path, but cannot contain userinfo, query or fragment. Inline token, basic or client-certificate credentials stay private. External certificate/key/token files, exec plugins, auth-provider plugins and extensions are unsupported and refused without following references or running commands. No default kubeconfig or host root store is used in place of this explicit profile.

An optional explicit proxy may use `http`, `https` or `socks5`, with a host and no userinfo, query or fragment. Proxy URL authentication is unsupported. Without an explicit proxy, nonempty `HTTP_PROXY`, `http_proxy`, `HTTPS_PROXY`, `https_proxy`, `NO_PROXY` and `no_proxy` are refused, so process environment cannot silently choose another route. Nonempty `KUBE_*` backend overrides are always refused in this mode. The same rules run before each local IaC subprocess, including observation and retained-provider initialization. Other provider environment is preserved; this is not complete cloud-identity attestation.

At explicit fresh PostgreSQL initialization, the selected cluster endpoint, inline CA bytes, TLS server name and proxy/transport settings enter the existing encrypted storage identity check. Normal workers compare that identity before recovery or worker activation. Source filename, aliases, comments, unrelated contexts and inline user credentials are not destination authority. CA byte equality is conservative, so even an equivalent reformatted CA bundle counts as a policy change.

Format5 refuses earlier Native storage formats instead of rewriting retained authority. A different destination/trust policy also refuses Open and Initialize. Do not edit metadata, regenerate the retained key or clear a fence to make a worker join. A supported custody-preserving destination handoff remains a separate operator lifecycle. The retained SQLite/v1 profile and its encryption/history behavior are unchanged.

Fresh execution stages a private `0600` snapshot at a fixed relative filename. Compiled bundles cannot supply that reserved file. Saved Apply first authenticates and restores the exact retained archive, checks its destination, then refreshes only the private configuration file using the current worker's same-target credentials. Original encrypted archive and selected saved-plan bytes stay unchanged. This allows another worker directory and refreshed token/certificate to apply the reviewed plan after the original source directory is gone.

Each running worker keeps the snapshot captured at startup. Provision a refreshed same-target credential on a new/restarted worker; this batch does not watch files or renew tokens live. It also does not qualify arbitrary kubeconfig plugins, other cloud backends' private routing or a remote cluster's identity. A Native Kubernetes request with no supported explicit snapshot refuses before initialization/effect. The public catalogue validates this local profile offline but does not expose or centrally attest its private destination.

To qualify the actual backend, supply an explicit admin configuration for an owned disposable test cluster:

```sh
NATIVE_BACKEND_ADMIN_CONFIG=/private/owned-test-admin.yaml make test-native-kubernetes-backend
```

The target requires real Terraform and OpenTofu and the exact test root, with no missing-environment skip. It creates only invocation-labelled namespaces, namespace-limited service accounts and state roles, then removes those namespaces. It never reads or changes the default kubecontext. The admin input must belong to the explicitly designated test cluster. The test proves source-directory removal, actual loss of old credential access, same-target saved Apply with unchanged plan hash, foreign target refusal, read-only observation, direct Apply and Destroy against real Kubernetes state storage.

The dedicated CI job creates its own Kind cluster using `v0.33.0` and Kubernetes `v1.36.4` with pinned official binary/image digests. It uses shipped Linux OpenTofu `1.13.2` and checksum-pinned Terraform `1.17.2`, with exact cluster cleanup on failure. Local Darwin qualification uses the installed versions recorded in its receipt; it does not substitute for the hosted Linux job or coordinated publication of required shared modules.
