# Official OpenTofu residuals accepted for Runner 3.2.0

On 3 November 2026 the release owner explicitly accepted the known residual
findings in the unchanged official vendor artifacts for this release, after
rechecking official releases, asset identities and image indexes. This is a risk
disposition, not a vulnerability fix or a claim that the findings are unreachable.

Runner uses the official signed OpenTofu 1.13.2 artifacts. The newer publication
date of the 1.12.11 maintenance release does not make it a 1.13 successor, and no
downgrade or custom vendor rebuild was selected. Upstream availability alone
does not establish security clearance.

The exception applies only to `v3.2.0` and canonical `v3.2.0-rc.N` releases. Every
other publisher version is rejected until its own disposition is reviewed.
Ordinary branch CI explicitly qualifies this release family; publication gets
its release identity from the actual stable tag or candidate workflow input.
There is no publisher fallback version.

| Official Linux artifact | Executable SHA256 |
| --- | --- |
| AMD64 | `949061cbb3c3d4558928ca9a459a5052b8c5e5545d17656f33316d2e3bc335f2` |
| ARM64 | `92fb091345bc9ba9339496663aac81bd9a3686969682d8142d4c08ce18d9b9a8` |

Both executables report Go 1.27.3. Their authenticated archive/checksum route uses
the existing expected OpenTofu primary signing key
`E3E6E43D84CB852EADB0051D0C0AF313E5FD9F80`. The normal installer signature and
checksum verification remain mandatory.

Pinned govulncheck 1.9.0 reports these 11 advisory IDs on both executables:

- GO-2026-5932
- GO-2026-6179
- GO-2026-6180
- GO-2026-6443
- GO-2026-6603
- GO-2026-6610
- GO-2026-6611
- GO-2026-6612
- GO-2026-6617
- GO-2026-6860
- GO-2026-6866

`scripts/vendorcheck/accepted-opentofu.json` binds each architecture and exact
executable hash to the compiler, complete advisory set and a canonical digest
of all 1,402 finding records. It covers module, package and symbol records,
every trace frame and fixed-version field, retaining duplicate records. Record
order is normalized. New, removed or changed findings reject the disposition.
The policy does not discard uncalled findings or infer exploitation from a
symbol finding.

`scripts/check-go-security.sh vendor-binary RELEASE PATH` prints both actual
text and full JSON reports. JSON's zero exit is not treated as a clean result:
the validator also requires the normal text finding exit, expected scanner
protocol/version/database/mode, complete report and exact accepted tuple.
Malformed/truncated JSON, duplicate keys, unknown messages, scanner errors,
changed executable bytes and out-of-scope versions fail closed.

Application source and the Runner executable retain their strict existing
checks. The final Docker image still copies both payloads only from the checked
security stage, refreshed for each published platform. The test-only legacy
engine remains outside the shipped-artifact exception.

This disposition does not extend to future releases, other vendor versions,
other executables, source vulnerabilities or private plugins. The release owner
must reassess a corrected official vendor artifact and any changed finding set.
