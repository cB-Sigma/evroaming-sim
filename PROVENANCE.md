# Provenance

## How the code was written

Written for SigmaM2M, a oneM2M platform, in Go, by the author of the
evroaming module and from the same public documents. No code was ported
or copied from another implementation.

`ocpisim.Sim`, `oicpsim` and `oscpsim` were first written next to
SigmaM2M's EV charging adapters to test them without partners: the OCPI
eMSP and hub from the OCPI 2.2.1 text and the OICP platform from
Hubject's public description of the CPO side, without reading the
adapter or library code, so that their agreement with it means
something; the OSCP party validates with the library's embedded OCA
schemas. They moved into the evroaming module on 2026-10-06 and from
there into this module on 2026-10-11 (with `git mv`), so the library
does not depend on its simulators.

Added for v0.1.0 (2026-10-11): `ocpisim.Party`, a party of any role on
the library's `ocpi.Router` serving every module (so it is not
independent of the library; `ocpisim.Sim` is); oicpsim's reservation
start and stop, per-operation refusals, HTTP faults and delays; oscpsim's
per-endpoint faults, delays and the handshake_acknowledgment spelling;
`ocpisim -full`; the tests at the module root that drive every
evroaming operation; the `interop` module.

## Sources

| Area (files) | Followed |
|---|---|
| sim/ocpisim/ocpisim.go | OCPI 2.2.1, github.com/ocpi/ocpi tag 2.2.1 (CC BY-ND 4.0), facts only |
| sim/ocpisim/party.go | the evroaming `ocpi` package (its router and client helpers) and OCPI 2.2.1 for the behaviour of results posted to response URLs |
| sim/oicpsim | OICP 2.3 CPO, github.com/hubject/oicp and github.com/hubject/oicp-cpo-2.3-api-doc (CC BY-SA 4.0), facts only |
| sim/oscpsim | OSCP 2.0 FINAL 2020-10-12 (CC BY-ND 4.0), facts only, and the OCA schemas through evroaming |
| cmd/ | none (flags are our own) |
| interop/peer/app.py | our own script running extrawest_ocpi 2025.7.16 (MIT) from PyPI; its store and authenticator interfaces were read for how to call them |

No specification or vendor text is reproduced.

## Tests

| Test | Source |
|---|---|
| ocpi_test.go, oicp_test.go, oscp_test.go, cmd/ | the evroaming packages against these simulators; written by the same author, not independent |
| interop/ | extrawest_ocpi 2025.7.16's client side against `ocpisim.Party` |

All tokens, identifiers and addresses are made-up demo values;
certificates and keys are made at run time.
