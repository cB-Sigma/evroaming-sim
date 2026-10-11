// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package interop

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cB-Sigma/evroaming-sim/sim/ocpisim"
	"github.com/cB-Sigma/evroaming/ocpi"
)

const ts = "2026-10-11T08:00:00Z"

// extrawest_ocpi 2025.7.16's client side against ocpisim.Party: the peer
// reads the party's versions and details when the party registers,
// POSTs command and charging profile results to the party's response
// URLs, and pushes locations, sessions, tariffs, tokens and CDRs to the
// party's Receiver interfaces with its push feature.
func TestPeerClientAgainstParty(t *testing.T) {
	needDocker(t)
	p := startPeer(t, "peer-token-a")
	party, err := ocpisim.NewParty(ocpisim.PartyConfig{Self: ocpi.Party{CountryCode: "NL", PartyID: "SIM"}, Role: ocpi.RoleEMSP,
		PublicHost: "host.docker.internal", Listen: "127.0.0.1:0", TrailingSlash: true})
	if err != nil {
		t.Fatal(err)
	}
	defer party.Close()
	ctx := context.Background()
	if err := party.Register(ctx, p.url()+"/versions", "peer-token-a"); err != nil {
		t.Fatalf("registration: %v\n%s", err, p.output())
	}
	if !p.saw("registered", "host.docker.internal") {
		t.Fatal("the peer did not read the party's versions and details")
	}
	t.Logf("passed: the peer read the party's versions and version details during registration")
	do := party.Doer()
	cpoBase := strings.TrimSuffix(party.PeerEndpoint(ocpi.ModLocations, ocpi.Sender), "locations/")
	emspLoc := party.PeerEndpoint(ocpi.ModLocations, ocpi.Receiver)
	peerParty := ocpi.Party{CountryCode: "BE", PartyID: "PER"}

	// Objects in the peer's store, for it to push back.
	loc := ocpi.Location{CountryCode: "BE", PartyID: "PER", ID: "L1", Publish: true, Address: "Demo Street 1", City: "Gent", Country: "BEL",
		Coordinates: ocpi.GeoLocation{Latitude: "51.054342", Longitude: "3.717424"}, TimeZone: "Europe/Brussels", LastUpdated: ts,
		EVSEs: []ocpi.EVSE{{UID: "L1-E1", Status: ocpi.EVSEAvailable, LastUpdated: ts, Connectors: []ocpi.Connector{{ID: "1",
			Standard: "IEC_62196_T2", Format: ocpi.FormatSocket, PowerType: ocpi.PowerAC3Phase, MaxVoltage: 230, MaxAmperage: 16, LastUpdated: ts}}}}}
	sess := ocpi.Session{CountryCode: "BE", PartyID: "PER", ID: "S1", StartDateTime: ts, KWh: 2, AuthMethod: ocpi.AuthCommand,
		CdrToken:   ocpi.CdrToken{CountryCode: "NL", PartyID: "SIM", UID: "T1", Type: ocpi.TokenRFID, ContractID: "NL-SIM-C1"},
		LocationID: "L1", EVSEUID: "L1-E1", ConnectorID: "1", Currency: "EUR", Status: ocpi.SessionActive, LastUpdated: ts}
	tariff := ocpi.Tariff{CountryCode: "BE", PartyID: "PER", ID: "TA1", Currency: "EUR", LastUpdated: ts,
		Elements: []ocpi.TariffElement{{PriceComponents: []ocpi.PriceComponent{{Type: ocpi.TariffTime, Price: 2, StepSize: 60}}}}}
	tok := ocpi.Token{CountryCode: "BE", PartyID: "PER", UID: "T1", Type: ocpi.TokenRFID, ContractID: "BE-PER-C1", Issuer: "Peer", Valid: true,
		Whitelist: ocpi.WhitelistAllowed, LastUpdated: ts}
	cdr := ocpi.CDR{CountryCode: "BE", PartyID: "PER", ID: "C1", StartDateTime: ts, EndDateTime: ts, CdrToken: sess.CdrToken, AuthMethod: ocpi.AuthCommand,
		CdrLocation: ocpi.CdrLocation{ID: "L1", Address: "Demo Street 1", City: "Gent", Country: "BEL", Coordinates: loc.Coordinates,
			EVSEUID: "L1-E1", EVSEID: "BE*PER*EL1", ConnectorID: "1", ConnectorStandard: "IEC_62196_T2", ConnectorFormat: ocpi.FormatSocket, ConnectorPowerType: ocpi.PowerAC3Phase},
		Currency: "EUR", ChargingPeriods: []ocpi.ChargingPeriod{{StartDateTime: ts, Dimensions: []ocpi.CdrDimension{{Type: ocpi.DimTime, Volume: 1}}}},
		TotalCost: ocpi.Price{ExclVAT: 2}, TotalEnergy: 2, TotalTime: 1, LastUpdated: ts}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v\n%s", what, err, p.output())
		}
	}
	must("seed location", ocpi.PutLocation(ctx, do, emspLoc, nil, peerParty, loc, "L1"))
	must("seed session", ocpi.PutSession(ctx, do, party.PeerEndpoint(ocpi.ModSessions, ocpi.Receiver), nil, peerParty, sess))
	must("seed tariff", ocpi.PutTariff(ctx, do, party.PeerEndpoint(ocpi.ModTariffs, ocpi.Receiver), nil, peerParty, tariff))
	must("seed token", ocpi.PutToken(ctx, do, party.PeerEndpoint(ocpi.ModTokens, ocpi.Receiver), nil, tok))
	_, err = ocpi.PostCDR(ctx, do, party.PeerEndpoint(ocpi.ModCDRs, ocpi.Receiver), nil, cdr)
	must("seed CDR", err)

	var cmdSender, cpSender string
	for _, e := range party.Router().Endpoints() {
		switch {
		case e.Identifier == ocpi.ModCommands && e.Role == ocpi.Sender:
			cmdSender = e.URL
		case e.Identifier == ocpi.ModChargingProfiles && e.Role == ocpi.Sender:
			cpSender = e.URL
		}
	}
	wait := func(k string) bool {
		for i := 0; i < 150; i++ {
			if party.Results()[k] != nil {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	// Command results.
	cr, err := ocpi.SendCommand(ctx, do, cpoBase+"commands", nil, ocpi.CmdStartSession,
		ocpi.StartSession{ResponseURL: cmdSender + "start-1", Token: tok, LocationID: "L1", EVSEUID: "L1-E1"})
	if err != nil || cr.Result != ocpi.CmdRespAccepted {
		t.Errorf("command: %+v %v", cr, err)
	}
	if wait("start-1") {
		t.Logf("passed: the peer POSTed a CommandResult to the party's commands Sender")
	} else {
		t.Errorf("no CommandResult reached the party")
	}

	// Charging profile results.
	cpR := party.PeerEndpoint(ocpi.ModChargingProfiles, ocpi.Receiver)
	if r, err := ocpi.GetActiveChargingProfile(ctx, do, strings.TrimSuffix(cpR, "/"), nil, "S1", 900, cpSender+"get-1"); err != nil || r.Result != ocpi.ProfileRespAccepted {
		t.Errorf("get profile: %+v %v", r, err)
	}
	if wait("get-1") {
		t.Logf("passed: the peer POSTed an ActiveChargingProfileResult to the party's chargingprofiles Sender")
	} else {
		t.Errorf("no ActiveChargingProfileResult reached the party")
	}
	if r, err := ocpi.ClearChargingProfile(ctx, do, strings.TrimSuffix(cpR, "/"), nil, "S1", cpSender+"clear-1"); err != nil || r.Result != ocpi.ProfileRespAccepted {
		t.Errorf("clear profile: %+v %v", r, err)
	}
	if wait("clear-1") {
		t.Logf("passed: the peer POSTed a ClearProfileResult to the party's chargingprofiles Sender")
	} else {
		t.Errorf("no ClearProfileResult reached the party")
	}

	// Pushes: the peer reads the party's version details with the
	// party's token and PUTs (CDRs: POSTs) each object to the party's
	// Receiver interface. The peer stores CiString IDs in lower case.
	tokC := party.PeerToken()
	for _, push := range []struct{ module, id, stored string }{
		{ocpi.ModLocations, "l1", "BE/PER/l1"},
		{ocpi.ModSessions, "s1", "BE/PER/s1"},
		{ocpi.ModTariffs, "ta1", "BE/PER/ta1"},
		{ocpi.ModTokens, "t1", "BE/PER/t1/RFID"},
		{ocpi.ModCDRs, "c1", "BE/PER/c1"},
	} {
		body, _ := json.Marshal(map[string]any{"module_id": push.module, "object_id": push.id,
			"receivers": []map[string]any{{"endpoints_url": party.Router().DetailsURL(), "auth_token": party.Token()}}})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(p.url(), "/ocpi")+"/push/2.2.1", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(tokC)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("push %s: %v", push.module, err)
			continue
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		got := party.Stored(push.module)
		if _, ok := got[push.stored]; ok {
			t.Logf("passed: the peer pushed %s %s to the party's Receiver", push.module, push.id)
		} else {
			keys := []string{}
			for k := range got {
				keys = append(keys, k)
			}
			t.Errorf("push %s: HTTP %d %s; the party holds %v", push.module, resp.StatusCode, rb, keys)
		}
	}
}
