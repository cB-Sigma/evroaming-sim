// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package evroamingsim_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/cB-Sigma/evroaming-sim/sim/ocpisim"
	"github.com/cB-Sigma/evroaming/ocpi"
)

var (
	cpoParty  = ocpi.Party{CountryCode: "NL", PartyID: "CPO"}
	emspParty = ocpi.Party{CountryCode: "DE", PartyID: "MSP"}
	ts        = "2026-10-11T08:00:00Z"
)

func location(id string) ocpi.Location {
	return ocpi.Location{CountryCode: "NL", PartyID: "CPO", ID: id, Publish: true, Address: "Demo Street 1", City: "Utrecht", Country: "NLD",
		Coordinates: ocpi.GeoLocation{Latitude: "52.090736", Longitude: "5.121420"}, TimeZone: "Europe/Amsterdam", LastUpdated: ts,
		OpeningTimes: &ocpi.Hours{RegularHours: []ocpi.RegularHours{{Weekday: 1, PeriodBegin: "08:00", PeriodEnd: "20:00"}}},
		EnergyMix:    &ocpi.EnergyMix{IsGreenEnergy: true, EnergySources: []ocpi.EnergySource{{Source: ocpi.SourceSolar, Percentage: 100}}},
		EVSEs: []ocpi.EVSE{{UID: id + "-E1", EVSEID: "NL*CPO*E" + id, Status: ocpi.EVSEAvailable, LastUpdated: ts,
			Capabilities: []string{ocpi.CapRemoteStartStopCapable, ocpi.CapChargingProfileCapable},
			Connectors: []ocpi.Connector{{ID: "1", Standard: "IEC_62196_T2", Format: ocpi.FormatSocket, PowerType: ocpi.PowerAC3Phase,
				MaxVoltage: 230, MaxAmperage: 32, LastUpdated: ts}}}}}
}

// ocpiPair starts a CPO and an eMSP; the eMSP registers with the CPO.
func ocpiPair(t *testing.T) (cpo, emsp *ocpisim.Party) {
	t.Helper()
	var err error
	cpo, err = ocpisim.NewParty(ocpisim.PartyConfig{Self: cpoParty, Role: ocpi.RoleCPO, TokenA: "token-a-for-msp"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cpo.Close)
	emsp, err = ocpisim.NewParty(ocpisim.PartyConfig{Self: emspParty, Role: ocpi.RoleEMSP})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(emsp.Close)
	if err := emsp.Register(context.Background(), cpo.VersionsURL(), "token-a-for-msp"); err != nil {
		t.Fatalf("registration: %v", err)
	}
	return cpo, emsp
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not happen", what)
}

// The credentials handshake both ways round: the eMSP registers, both
// know each other's endpoints for every module and role, the registration
// token stops working for modules, credentials can be read, updated and
// deleted.
func TestOCPICredentials(t *testing.T) {
	cpo, emsp := ocpiPair(t)
	ctx := context.Background()
	if n := len(emsp.PeerEndpoints()); n != 17 {
		t.Errorf("the eMSP knows %d of the CPO's endpoints", n)
	}
	if n := len(cpo.PeerEndpoints()); n != 17 {
		t.Errorf("the CPO knows %d of the eMSP's endpoints", n)
	}
	do := emsp.Doer()
	cred := emsp.PeerEndpoint(ocpi.ModCredentials, ocpi.Sender)
	theirs, err := ocpi.GetCredentials(ctx, do, cred)
	if err != nil || theirs.Roles[0].Role != ocpi.RoleCPO {
		t.Errorf("GET credentials %+v %v", theirs, err)
	}
	// Token A no longer opens functional modules.
	var c ocpi.Client
	_, err = c.Send(ctx, ocpi.Request{Method: http.MethodGet, URL: emsp.PeerEndpoint(ocpi.ModLocations, ocpi.Sender)}, "token-a-for-msp", false)
	var ce *ocpi.CallError
	if !errors.As(err, &ce) || ce.HTTP != http.StatusUnauthorized {
		t.Errorf("token A on a module: %v", err)
	}
	// A second POST is refused; PUT updates and changes the token.
	if _, err := ocpi.RegisterCredentials(ctx, do, cred, "", ocpi.Credentials{Token: "x", URL: emsp.VersionsURL()}); err == nil {
		t.Error("a second registration was accepted")
	}
	upd, err := ocpi.UpdateCredentials(ctx, do, cred, ocpi.Credentials{Token: "new-token-b", URL: emsp.VersionsURL(),
		Roles: []ocpi.CredentialsRole{{Role: ocpi.RoleEMSP, PartyID: "MSP", CountryCode: "DE", BusinessDetails: ocpi.BusinessDetails{Name: "MSP"}}}})
	if err != nil || upd.Token == theirs.Token {
		t.Errorf("PUT credentials %+v %v", upd, err)
	}
	if err := ocpi.Unregister(ctx, do, cred); err == nil {
		t.Error("DELETE with the old token succeeded")
	}
}

// Every module operation, from both sides, through the library's client
// helpers and its Router.
func TestOCPIEveryModule(t *testing.T) {
	cpo, emsp := ocpiPair(t)
	ctx := context.Background()
	toCPO, toMSP := &cpoParty, &emspParty
	md, cd := emsp.Doer(), cpo.Doer()
	ep := func(p *ocpisim.Party, mod, role string) string {
		u := p.PeerEndpoint(mod, role)
		if u == "" {
			t.Fatalf("no %s %s endpoint", mod, role)
		}
		return u
	}

	// Locations: the CPO's Sender, paged.
	for _, id := range []string{"L1", "L2", "L3"} {
		cpo.Own(ocpi.ModLocations, id, location(id))
	}
	locs, total, err := ocpi.GetLocations(ctx, md, ep(emsp, ocpi.ModLocations, ocpi.Sender), toCPO, nil)
	if err != nil || total != 3 || len(locs) != 3 || locs[0].EnergyMix == nil {
		t.Fatalf("locations %d of %d: %v", len(locs), total, err)
	}
	var conn ocpi.Connector
	if err := ocpi.GetLocation(ctx, md, ep(emsp, ocpi.ModLocations, ocpi.Sender), toCPO, &conn, "L2", "L2-E1", "1"); err != nil || conn.MaxAmperage != 32 {
		t.Errorf("connector %+v %v", conn, err)
	}
	if err := ocpi.GetLocation(ctx, md, ep(emsp, ocpi.ModLocations, ocpi.Sender), toCPO, &conn, "L9"); !ocpi.NotFound(err) {
		t.Errorf("unknown location: %v", err)
	}
	// Locations: the eMSP's Receiver.
	locR := ep(cpo, ocpi.ModLocations, ocpi.Receiver)
	if err := ocpi.PutLocation(ctx, cd, locR, toMSP, cpoParty, location("L1"), "L1"); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.PatchLocation(ctx, cd, locR, toMSP, cpoParty, map[string]any{"name": "Patched", "last_updated": ts}, "L1"); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.PutLocation(ctx, cd, locR, toMSP, cpoParty, location("L1").EVSEs[0], "L1", "L1-E1"); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.PatchLocation(ctx, cd, locR, toMSP, cpoParty, map[string]any{"name": "x"}, "L1"); err == nil {
		t.Error("a PATCH without last_updated was sent")
	}
	if got := emsp.Stored(ocpi.ModLocations)["NL/CPO/L1"].(map[string]any); got["name"] != "Patched" {
		t.Errorf("stored location %v", got)
	}
	var back ocpi.EVSE
	if err := ocpi.GetLocation(ctx, cd, locR, toMSP, &back, "NL", "CPO", "L1", "L1-E1"); err != nil || back.UID != "L1-E1" {
		t.Errorf("receiver GET %+v %v", back, err)
	}

	// Sessions both ways, with charging preferences.
	sess := ocpi.Session{CountryCode: "NL", PartyID: "CPO", ID: "S1", StartDateTime: ts, KWh: 1.5, AuthMethod: ocpi.AuthCommand,
		CdrToken:   ocpi.CdrToken{CountryCode: "DE", PartyID: "MSP", UID: "T1", Type: ocpi.TokenAppUser, ContractID: "DE-MSP-C1"},
		LocationID: "L1", EVSEUID: "L1-E1", ConnectorID: "1", Currency: "EUR", Status: ocpi.SessionActive, LastUpdated: ts}
	cpo.Own(ocpi.ModSessions, "S1", sess)
	ss, _, err := ocpi.GetSessions(ctx, md, ep(emsp, ocpi.ModSessions, ocpi.Sender), toCPO, url.Values{"date_from": {"2026-01-01T00:00:00Z"}})
	if err != nil || len(ss) != 1 {
		t.Errorf("sessions %v %v", ss, err)
	}
	res, err := ocpi.SetChargingPreferences(ctx, md, ep(emsp, ocpi.ModSessions, ocpi.Sender), toCPO, "S1", ocpi.ChargingPreferences{ProfileType: ocpi.ProfileFast})
	if err != nil || res != ocpi.PrefsAccepted {
		t.Errorf("preferences %q %v", res, err)
	}
	if err := ocpi.PutSession(ctx, cd, ep(cpo, ocpi.ModSessions, ocpi.Receiver), toMSP, cpoParty, sess); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.PatchSession(ctx, cd, ep(cpo, ocpi.ModSessions, ocpi.Receiver), toMSP, cpoParty, "S1", map[string]any{"kwh": 3.0, "last_updated": ts}); err != nil {
		t.Fatal(err)
	}
	got, err := ocpi.Get[ocpi.Session](ctx, cd, ocpi.ObjectURL(ep(cpo, ocpi.ModSessions, ocpi.Receiver), cpoParty, "S1"), toMSP)
	if err != nil || got.KWh != 3 {
		t.Errorf("session back %+v %v", got, err)
	}

	// CDRs: POST with the Location header, read back; the Sender list.
	cdr := ocpi.CDR{CountryCode: "NL", PartyID: "CPO", ID: "C1", StartDateTime: ts, EndDateTime: ts, CdrToken: sess.CdrToken, AuthMethod: ocpi.AuthCommand,
		CdrLocation: ocpi.CdrLocation{ID: "L1", Address: "a", City: "c", Country: "NLD", EVSEUID: "L1-E1", EVSEID: "NL*CPO*EL1", ConnectorID: "1",
			ConnectorStandard: "IEC_62196_T2", ConnectorFormat: ocpi.FormatSocket, ConnectorPowerType: ocpi.PowerAC3Phase},
		Currency: "EUR", ChargingPeriods: []ocpi.ChargingPeriod{{StartDateTime: ts, Dimensions: []ocpi.CdrDimension{{Type: ocpi.DimEnergy, Volume: 3}}}},
		TotalCost: ocpi.Price{ExclVAT: 1.2}, TotalEnergy: 3, TotalTime: 0.5, LastUpdated: ts,
		SignedData: &ocpi.SignedData{EncodingMethod: "OCMF", SignedValues: []ocpi.SignedValue{{Nature: "Start", PlainData: "p", SignedData: "s"}}}}
	loc, err := ocpi.PostCDR(ctx, cd, ep(cpo, ocpi.ModCDRs, ocpi.Receiver), toMSP, cdr)
	if err != nil || loc == "" {
		t.Fatalf("CDR post %q %v", loc, err)
	}
	if c2, err := ocpi.GetCDR(ctx, cd, loc, toMSP); err != nil || c2.SignedData == nil {
		t.Errorf("CDR back %+v %v", c2, err)
	}
	if _, err := ocpi.PostCDR(ctx, cd, ep(cpo, ocpi.ModCDRs, ocpi.Receiver), toMSP, cdr); err == nil {
		t.Error("a duplicate CDR was accepted")
	}
	cpo.Own(ocpi.ModCDRs, "C1", cdr)
	if cs, _, err := ocpi.GetCDRs(ctx, md, ep(emsp, ocpi.ModCDRs, ocpi.Sender), toCPO, nil); err != nil || len(cs) != 1 {
		t.Errorf("CDR list %v %v", cs, err)
	}

	// Tariffs both ways, with DELETE.
	tariff := ocpi.Tariff{CountryCode: "NL", PartyID: "CPO", ID: "TA1", Currency: "EUR", LastUpdated: ts,
		Elements: []ocpi.TariffElement{{PriceComponents: []ocpi.PriceComponent{{Type: ocpi.TariffEnergy, Price: 0.3, StepSize: 1}}}}}
	cpo.Own(ocpi.ModTariffs, "TA1", tariff)
	if tl, _, err := ocpi.GetTariffs(ctx, md, ep(emsp, ocpi.ModTariffs, ocpi.Sender), toCPO, nil); err != nil || len(tl) != 1 {
		t.Errorf("tariffs %v %v", tl, err)
	}
	tr := ep(cpo, ocpi.ModTariffs, ocpi.Receiver)
	if err := ocpi.PutTariff(ctx, cd, tr, toMSP, cpoParty, tariff); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.DeleteTariff(ctx, cd, tr, toMSP, cpoParty, "TA1"); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.DeleteTariff(ctx, cd, tr, toMSP, cpoParty, "TA1"); !ocpi.NotFound(err) {
		t.Errorf("second DELETE: %v", err)
	}

	// Tokens: the eMSP's Sender (list, real-time authorisation) and the
	// CPO's Receiver (PUT, PATCH, GET with type).
	tok := ocpi.Token{CountryCode: "DE", PartyID: "MSP", UID: "T1", Type: ocpi.TokenRFID, ContractID: "DE-MSP-C1", Issuer: "MSP", Valid: true,
		Whitelist: ocpi.WhitelistAllowed, LastUpdated: ts}
	emsp.Own(ocpi.ModTokens, "T1", tok)
	blocked := tok
	blocked.UID, blocked.Valid = "T2", false
	emsp.Own(ocpi.ModTokens, "T2", blocked)
	if tl, _, err := ocpi.GetTokens(ctx, cd, ep(cpo, ocpi.ModTokens, ocpi.Sender), toMSP, nil); err != nil || len(tl) != 2 {
		t.Errorf("tokens %v %v", tl, err)
	}
	ai, err := ocpi.AuthorizeToken(ctx, cd, ep(cpo, ocpi.ModTokens, ocpi.Sender), toMSP, "T1", ocpi.TokenRFID, &ocpi.LocationReferences{LocationID: "L1"})
	if err != nil || ai.Allowed != ocpi.AllowedAllowed || ai.Location == nil {
		t.Errorf("authorise %+v %v", ai, err)
	}
	if ai, err := ocpi.AuthorizeToken(ctx, cd, ep(cpo, ocpi.ModTokens, ocpi.Sender), toMSP, "T2", "", nil); err != nil || ai.Allowed != ocpi.AllowedBlocked {
		t.Errorf("blocked token %+v %v", ai, err)
	}
	if _, err := ocpi.AuthorizeToken(ctx, cd, ep(cpo, ocpi.ModTokens, ocpi.Sender), toMSP, "T9", "", nil); !ocpi.NotFound(err) {
		t.Errorf("unknown token: %v", err)
	}
	tR := ep(emsp, ocpi.ModTokens, ocpi.Receiver)
	if err := ocpi.PutToken(ctx, md, tR, toCPO, tok); err != nil {
		t.Fatal(err)
	}
	if err := ocpi.PatchToken(ctx, md, tR, toCPO, emspParty, "T1", ocpi.TokenRFID, map[string]any{"valid": false, "last_updated": ts}); err != nil {
		t.Fatal(err)
	}
	if back, err := ocpi.Get[ocpi.Token](ctx, md, ocpi.TokenURL(tR, emspParty, "T1", ocpi.TokenRFID), toCPO); err != nil || back.Valid {
		t.Errorf("token back %+v %v", back, err)
	}

	// Commands: accepted, then the result arrives at the eMSP's Sender.
	cmdEP := ep(emsp, ocpi.ModCommands, ocpi.Receiver)
	resultBase := emsp.Router().Endpoints()
	var cmdSender string
	for _, e := range resultBase {
		if e.Identifier == ocpi.ModCommands && e.Role == ocpi.Sender {
			cmdSender = e.URL
		}
	}
	for i, c := range []struct {
		name string
		body any
	}{
		{ocpi.CmdStartSession, ocpi.StartSession{ResponseURL: cmdSender + "/START_SESSION/1", Token: tok, LocationID: "L1", EVSEUID: "L1-E1"}},
		{ocpi.CmdStopSession, ocpi.StopSession{ResponseURL: cmdSender + "/STOP_SESSION/2", SessionID: "S1"}},
		{ocpi.CmdReserveNow, ocpi.ReserveNow{ResponseURL: cmdSender + "/RESERVE_NOW/3", Token: tok, ExpiryDate: ts, ReservationID: "R1", LocationID: "L1"}},
		{ocpi.CmdCancelReservation, ocpi.CancelReservation{ResponseURL: cmdSender + "/CANCEL_RESERVATION/4", ReservationID: "R1"}},
		{ocpi.CmdUnlockConnector, ocpi.UnlockConnector{ResponseURL: cmdSender + "/UNLOCK_CONNECTOR/5", LocationID: "L1", EVSEUID: "L1-E1", ConnectorID: "1"}},
	} {
		cr, err := ocpi.SendCommand(ctx, md, cmdEP, toCPO, c.name, c.body)
		if err != nil || cr.Result != ocpi.CmdRespAccepted {
			t.Errorf("%s: %+v %v", c.name, cr, err)
		}
		k := fmt.Sprintf("%s/%d", c.name, i+1)
		waitFor(t, "the "+c.name+" result", func() bool { return emsp.Results()[k] != nil })
	}
	if cr, err := ocpi.SendCommand(ctx, md, cmdEP, toCPO, ocpi.CmdStartSession, ocpi.StartSession{ResponseURL: cmdSender + "/x", Token: tok, LocationID: "L9"}); !ocpi.NotFound(err) {
		t.Errorf("unknown location: %+v %v", cr, err)
	}
	if cr, err := ocpi.SendCommand(ctx, md, cmdEP, toCPO, ocpi.CmdStopSession, ocpi.StopSession{ResponseURL: cmdSender + "/y", SessionID: "S9"}); err != nil || cr.Result != ocpi.CmdRespUnknownSession {
		t.Errorf("unknown session: %+v %v", cr, err)
	}

	// Charging profiles: the eMSP as sender of profiles calls the CPO's
	// Receiver; results come back to its Sender; the CPO pushes an
	// active profile.
	cpR := ep(emsp, ocpi.ModChargingProfiles, ocpi.Receiver)
	var cpSender string
	for _, e := range emsp.Router().Endpoints() {
		if e.Identifier == ocpi.ModChargingProfiles && e.Role == ocpi.Sender {
			cpSender = e.URL
		}
	}
	limit := 3600
	r1, err := ocpi.SetChargingProfileFor(ctx, md, cpR, toCPO, "S1", ocpi.SetChargingProfile{ResponseURL: cpSender + "/set/1",
		ChargingProfile: ocpi.ChargingProfile{ChargingRateUnit: ocpi.RateWatts, Duration: &limit, ChargingProfilePeriod: []ocpi.ChargingProfilePeriod{{StartPeriod: 0, Limit: 7400}}}})
	if err != nil || r1.Result != ocpi.ProfileRespAccepted {
		t.Errorf("set profile %+v %v", r1, err)
	}
	waitFor(t, "the set result", func() bool { return emsp.Results()["set/1"] != nil })
	r2, err := ocpi.GetActiveChargingProfile(ctx, md, cpR, toCPO, "S1", 900, cpSender+"/get/1")
	if err != nil || r2.Result != ocpi.ProfileRespAccepted {
		t.Errorf("get profile %+v %v", r2, err)
	}
	waitFor(t, "the active profile", func() bool { return emsp.Results()["get/1"] != nil })
	if p, _ := emsp.Results()["get/1"].(map[string]any); p["profile"] == nil {
		t.Errorf("active profile result %v", p)
	}
	r3, err := ocpi.ClearChargingProfile(ctx, md, cpR, toCPO, "S1", cpSender+"/clear/1")
	if err != nil || r3.Result != ocpi.ProfileRespAccepted {
		t.Errorf("clear profile %+v %v", r3, err)
	}
	waitFor(t, "the clear result", func() bool { return emsp.Results()["clear/1"] != nil })
	if r, err := ocpi.GetActiveChargingProfile(ctx, md, cpR, toCPO, "S9", 900, cpSender+"/get/2"); err != nil || r.Result != ocpi.ProfileRespUnknownSession {
		t.Errorf("unknown session %+v %v", r, err)
	}
	if err := ocpi.PutActiveChargingProfile(ctx, cd, ep(cpo, ocpi.ModChargingProfiles, ocpi.Sender), toMSP, "S1",
		ocpi.ActiveChargingProfile{StartDateTime: ts, ChargingProfile: ocpi.ChargingProfile{ChargingRateUnit: ocpi.RateAmperes}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := emsp.Profile("S1").(ocpi.ActiveChargingProfile); !ok {
		t.Errorf("active profile at the eMSP %v", emsp.Profile("S1"))
	}

	// Hub client info: PUT to a Receiver, GET it back, and a hub's list.
	ci := ocpi.ClientInfo{PartyID: "XYZ", CountryCode: "BE", Role: ocpi.RoleCPO, Status: ocpi.ConnConnected, LastUpdated: ts}
	if err := ocpi.PutClientInfo(ctx, md, ep(emsp, ocpi.ModHubClientInfo, ocpi.Receiver), ci); err != nil {
		t.Fatal(err)
	}
	if back, err := ocpi.Get[ocpi.ClientInfo](ctx, md, ocpi.ObjectURL(ep(emsp, ocpi.ModHubClientInfo, ocpi.Receiver), ocpi.Party{CountryCode: "BE", PartyID: "XYZ"}), nil); err != nil || back.Status != ocpi.ConnConnected {
		t.Errorf("client info back %+v %v", back, err)
	}
	cpo.Own(ocpi.ModHubClientInfo, "BE/XYZ", ci)
	if cis, _, err := ocpi.GetClientInfos(ctx, md, ep(emsp, ocpi.ModHubClientInfo, ocpi.Sender), nil); err != nil || len(cis) != 1 {
		t.Errorf("client infos %v %v", cis, err)
	}
}

// Routing headers: the request carries OCPI-from and OCPI-to; the answer
// swaps them, and carries the request and correlation IDs back.
func TestOCPIRoutingHeaders(t *testing.T) {
	cpo, emsp := ocpiPair(t)
	cpo.Own(ocpi.ModLocations, "L1", location("L1"))
	rep, err := emsp.Doer()(context.Background(), ocpi.Request{Method: http.MethodGet, URL: emsp.PeerEndpoint(ocpi.ModLocations, ocpi.Sender),
		To: &cpoParty, Correlation: "corr-1"})
	if err != nil {
		t.Fatal(err)
	}
	h := rep.Header
	if h.Get("OCPI-to-party-id") != "MSP" || h.Get("OCPI-from-party-id") != "CPO" || h.Get("X-Correlation-ID") != "corr-1" || h.Get("X-Request-ID") == "" {
		t.Errorf("answer headers %v", h)
	}
}

// Faults: a module answering an error status, a slow module, and
// unregistering.
func TestOCPIFaults(t *testing.T) {
	cpo, emsp := ocpiPair(t)
	ctx := context.Background()
	md := emsp.Doer()
	u := emsp.PeerEndpoint(ocpi.ModLocations, ocpi.Sender)
	cpo.SetFault(ocpi.ModLocations, ocpisim.Fault{HTTP: http.StatusServiceUnavailable, Code: ocpi.StatusServerError})
	_, _, err := ocpi.GetLocations(ctx, md, u, &cpoParty, nil)
	var ce *ocpi.CallError
	if !errors.As(err, &ce) || ce.HTTP != http.StatusServiceUnavailable || ce.Code != ocpi.StatusServerError {
		t.Errorf("fault: %v", err)
	}
	cpo.SetFault(ocpi.ModLocations, ocpisim.Fault{Code: ocpi.StatusHubUnknownTo})
	if _, _, err := ocpi.GetLocations(ctx, md, u, &cpoParty, nil); !errors.As(err, &ce) || ce.Code != ocpi.StatusHubUnknownTo {
		t.Errorf("hub fault: %v", err)
	}
	cpo.SetFault(ocpi.ModLocations, ocpisim.Fault{Delay: 300 * time.Millisecond, Code: ocpi.StatusServerError})
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if _, _, err := ocpi.GetLocations(short, md, u, &cpoParty, nil); err == nil {
		t.Error("a slow partner did not time out")
	}
	cancel()
	cpo.SetFault(ocpi.ModLocations, ocpisim.Fault{})
	if _, _, err := ocpi.GetLocations(ctx, md, u, &cpoParty, nil); err != nil {
		t.Errorf("after the fault: %v", err)
	}
	cpo.SetCommandResult("")
	if err := ocpi.Unregister(ctx, md, emsp.PeerEndpoint(ocpi.ModCredentials, ocpi.Sender)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ocpi.GetLocations(ctx, md, u, &cpoParty, nil); !errors.As(err, &ce) || ce.HTTP != http.StatusUnauthorized {
		t.Errorf("after unregistering: %v", err)
	}
}
