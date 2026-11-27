# Kubernetes agent runner chart 0.5.0-rc.6

This Helm release candidate defaults to Runner `v3.4.0-rc.6` when
`image.tag` is empty. Install the chart with `--version 0.5.0-rc.6`.
Use the coordinated compatible backend release candidates for Native mode.

The Runner image is about 40% smaller to download. Templates and values are
unchanged from `0.5.0-rc.5`; only the default image moves to `v3.4.0-rc.6`.
Native sites update their execution catalogue from the rc.6 image while the
rc.5 workers are stopped, before starting rc.6, as the
[Runner 3.4.0-rc.6 notes](v3.4.0-rc.6.md) describe.
