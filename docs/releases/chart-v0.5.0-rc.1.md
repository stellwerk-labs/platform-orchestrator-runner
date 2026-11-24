# Kubernetes agent runner chart 0.5.0-rc.1

This Helm release candidate defaults to Runner `v3.4.0-rc.1` when
`image.tag` is empty. Install the chart with `--version 0.5.0-rc.1`.
Use the coordinated compatible backend release candidates for Native mode.

Native diode reverse imports bind each site to its organization, server-issued
Runner UUID and verification key. Follow the documented
[consumer cutover](../native-diode-ingress.md#existing-native-exporter-cutover)
when upgrading an existing Native diode exporter to preserve queued commands.
