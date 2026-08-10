# Kubernetes agent runner chart 0.2.0

This breaking chart release deploys runner `v2.0.0` and replaces the old private
key and direct HTTP runner configuration with NATS transport settings.

Configure `nats.url` and a token, NATS credentials file, or mTLS client
credentials. Edge mode also requires a `ReadWriteMany` storage class for its
persistent outbound spool. The optional protected-side NATS and diode relay
templates are included for the air-gap design, but the physical diode path was
not qualified in this release.
