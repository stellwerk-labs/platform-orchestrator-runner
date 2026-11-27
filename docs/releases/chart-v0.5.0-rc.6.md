# Kubernetes agent runner chart 0.5.0-rc.6

This Helm release candidate defaults to Runner `v3.4.0-rc.6` when
`image.tag` is empty. Install the chart with `--version 0.5.0-rc.6`.
Use the coordinated compatible backend release candidates for Native mode.

The Runner image is about 40% smaller to download; templates, values and
defaults are unchanged from `0.5.0-rc.5`. Native sites upgrading from
`0.5.0-rc.5` update their execution catalogue first, as the
[Runner 3.4.0-rc.6 notes](v3.4.0-rc.6.md) describe.
