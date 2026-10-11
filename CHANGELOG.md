# Changelog

All notable changes to this module. Versions follow semantic versioning;
before v1 the API may change in minor versions.

## v0.1.0 (2026-10-11)

First release as a module of its own, split from the evroaming module;
needs evroaming v0.1.0.

- `ocpisim.Sim` (an OCPI eMSP or hub), `oicpsim` (a Hubject-like OICP
  platform with a run-time PKI), `oscpsim` (an OSCP party of any role)
  and their commands, as they were in evroaming.
- `ocpisim.Party`: a party of any role serving every OCPI 2.2.1 module
  in both interface roles on `ocpi.Router`, with registration both
  ways, results POSTed to response URLs, fault switches per module
  (`SetFault`, `SetCommandResult`), `PublicHost` and `TrailingSlash`;
  `ocpisim -full`.
- oicpsim: eRoamingAuthorizeRemoteReservationStart and Stop sent to a
  CPO (`ReserveStart`, `ReserveStop`, `Reservations`); `Refuse` per
  operation, `SetHTTPStatus`, `SetDelay`.
- oscpsim: `Fail` per endpoint, `SetDelay`; the handshake_acknowledgment
  spelling is accepted.
- Tests at the module root drive every evroaming operation against the
  simulators, faults included.
- Interop: extrawest_ocpi 2025.7.16's client side against
  `ocpisim.Party`, in a nested test-only module, `interop`.
