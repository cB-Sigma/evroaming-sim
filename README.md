# evroaming-sim

Simulated EV charging back-office partners for testing OCPI 2.2.1, OICP 2.3 and OSCP 2.0 clients and servers without a roaming partner, Hubject or a grid operator; not affiliated with the EVRoaming Foundation, Hubject or the Open Charge Alliance.

[![Go Reference](https://pkg.go.dev/badge/github.com/cB-Sigma/evroaming-sim.svg)](https://pkg.go.dev/github.com/cB-Sigma/evroaming-sim) [![test](https://github.com/cB-Sigma/evroaming-sim/actions/workflows/test.yml/badge.svg)](https://github.com/cB-Sigma/evroaming-sim/actions/workflows/test.yml) [![Licence: Apache-2.0](https://img.shields.io/badge/licence-Apache--2.0-blue.svg)](LICENSE) [![Go](https://img.shields.io/badge/go-1.26-00ADD8.svg)](go.mod)

Three simulator packages and their commands: `ocpisim`, an OCPI partner
(an eMSP or hub written without the library, and a party of any role
serving every module on the library's router); `oicpsim`, a Hubject-like
OICP platform over mutual TLS with a throw-away certificate authority;
`oscpsim`, an OSCP party of any role. Each has fault switches. It is
built on [evroaming](https://github.com/cB-Sigma/evroaming). Written for
SigmaM2M, a oneM2M platform; it has no dependency on it.

## What is simulated

| Feature | Status |
|---|---|
| OCPI eMSP or hub (`ocpisim.Sim`): written from the OCPI text without the library; credentials both ways, Base64 or raw tokens, registration token limited to versions and credentials, paged lists, tokens with real-time authorisation, commands with results, sessions, CDRs and location updates received, a hub's locations and client info | yes |
| OCPI party of any role (`ocpisim.Party`): every module in both interface roles on `ocpi.Router` (17 endpoints), registering with a partner or being registered with, objects it owns served from its Sender interfaces, what partners push kept per Receiver interface, commands and charging profile requests answered with results POSTed to the response_url, charging preferences | yes |
| OCPI faults: an HTTP status and OCPI status code per module, slow answers (`Party.SetFault`), no command result (`SetCommandResult("")`), unknown objects, unknown sessions and locations in commands, a second registration, unregistering | yes |
| OICP platform (`oicpsim`): every CPO operation (EVSE data and status, pricing products, EVSE pricing, authorise start and stop, CDR, the four charging notifications) checked for required fields, identifier forms and the OperatorID of the client certificate; remote start and stop and reservation start and stop sent to a CPO | yes |
| OICP faults: refusal with a status code per operation (`Refuse`), HTTP errors such as 429 or 500 (`SetHTTPStatus`), the platform down (`SetDown`), slow answers (`SetDelay`), refused CDRs, cards that are not authorised | yes |
| OSCP party (`oscpsim`) of role CP, FP or CO: every message of 4.2 validated against the OCA schemas, Register and Handshake answered with their own later POST, heartbeats, segmented messages, the handshake_acknowledgment spelling | yes |
| OSCP faults: refused handshakes, stopped heartbeats, an HTTP status per endpoint (`Fail`), slow answers (`SetDelay`), unknown tokens, invalid messages | yes |
| Hub forwarding between several parties, OICP's EMP side, Hubject's Plug&Charge protocol | no |

## Install

```
go get github.com/cB-Sigma/evroaming-sim@latest
```

Requires Go 1.26 or later. The commands:

```
go install github.com/cB-Sigma/evroaming-sim/cmd/ocpisim@latest
go install github.com/cB-Sigma/evroaming-sim/cmd/oicpsim@latest
go install github.com/cB-Sigma/evroaming-sim/cmd/oscpsim@latest
```

## Command line

```
ocpisim -listen 127.0.0.1:9080 -country NL -party EMS -token-a demo-token-a
ocpisim -full -role CPO -country NL -party CPO -token-a demo-token-a
oicpsim -operator DE*ABC -out ./pki
oscpsim -role cp -token demo-token
oscpsim -role fp -peer http://127.0.0.1:9090/oscp/cp/2.0 -peer-token demo-token
```

| Command | Flags |
|---|---|
| `ocpisim` | `-listen`, `-role` (EMSP or HUB; with `-full` also CPO or SCSP), `-country`, `-party`, `-token-a`, `-raw`, `-page`, `-register` and `-cpo-token` (register with a partner), `-full` (every module, `ocpisim.Party`) |
| `oicpsim` | `-operator` (the CPO's OperatorID), `-out` (where the CA, the CPO's certificate and key are written), `-listen` |
| `oscpsim` | `-role` (cp, fp or co), `-token`, `-peer` and `-peer-token` (register with a party), `-handshake`, `-heartbeat`, `-listen` |

`ocpisim` prints its versions URL and registration token; `oicpsim`
writes a demo certificate authority and client certificate made at run
time, valid for a day.

## In Go tests

```go
cpo, _ := ocpisim.NewParty(ocpisim.PartyConfig{Self: ocpi.Party{CountryCode: "NL", PartyID: "CPO"}, Role: ocpi.RoleCPO, TokenA: "token-a"})
defer cpo.Close()
cpo.Own(ocpi.ModLocations, "L1", location)
emsp, _ := ocpisim.NewParty(ocpisim.PartyConfig{Self: ocpi.Party{CountryCode: "DE", PartyID: "MSP"}, Role: ocpi.RoleEMSP})
_ = emsp.Register(ctx, cpo.VersionsURL(), "token-a")
locs, _, _ := ocpi.GetLocations(ctx, emsp.Doer(), emsp.PeerEndpoint(ocpi.ModLocations, ocpi.Sender), nil, nil)
```

The tests at the module root (ocpi_test.go, oicp_test.go, oscp_test.go)
drive every evroaming operation against the simulators.

## Documentation

Reference: https://pkg.go.dev/github.com/cB-Sigma/evroaming-sim

| Package | What it does |
|---|---|
| `sim/ocpisim` | `Start`/`Sim` (an eMSP or hub without the library) and `NewParty`/`Party` (any role, every module, faults) |
| `sim/oicpsim` | `Start`/`Sim`, `NewPKI`/`PKI`, `Card`; `RemoteStart`, `RemoteStop`, `ReserveStart`, `ReserveStop`; fault switches |
| `sim/oscpsim` | `New`/`Party` of any role; `Register`, `Handshake`, `Send`, `Heartbeats`, `Post`; fault switches |
| `cmd/ocpisim`, `cmd/oicpsim`, `cmd/oscpsim` | the commands above |
| `interop` (nested module, tests only) | extrawest_ocpi's client side against `ocpisim.Party`, in a container |

Changes per version are in [CHANGELOG.md](CHANGELOG.md).

## Testing

```
go test -race ./...
cd interop && SIGMA_EVROAMING_INTEROP=1 go test -v ./...   # needs Docker
```

### Interop tested against

- extrawest_ocpi 2025.7.16's client side (MIT,
  github.com/extrawest/extrawest_ocpi), 2026-10-11, python:3.11.9-slim
  in Docker Desktop on arm64 (`interop`), against `ocpisim.Party` as an
  eMSP: the peer read the party's versions and version details during
  registration; POSTed a CommandResult, an ActiveChargingProfileResult
  and a ClearProfileResult to the party's response URLs; and with its
  push feature PUT a location, a session, a tariff and a token and
  POSTed a CDR to the party's Receiver interfaces. All passed. The push
  feature appends object paths to endpoint URLs without a "/", so the
  party publishes its URLs with a trailing slash (`TrailingSlash`).
- Not yet: an OICP platform other than this simulator (Hubject's QA
  service needs a contract), an OSCP implementation, and the
  `ocpisim.Sim` eMSP against an outside CPO.
- The simulators and the evroaming packages are written by the same
  author, so their agreement is not independent; the run above is.

## Demo values

Every token, party ID, operator ID, EVSE ID, contract ID, certificate
and key in this module is a made-up demo value or made at run time. Do
not use them for real systems.

## Related repositories

- [evroaming](https://github.com/cB-Sigma/evroaming): the protocol
  packages these simulators speak.

## Status

v0.x: the API may change before v1.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md).

## Licence

Apache-2.0 (see [LICENSE](LICENSE) and [NOTICE](NOTICE)). Copyright 2026
Abhinav Gautam. OCPI is a protocol of the EVRoaming Foundation, OICP and
Hubject are trademarks of Hubject GmbH, OSCP is a protocol of the Open
Charge Alliance; the names are used only to say which protocols these
simulators speak.
