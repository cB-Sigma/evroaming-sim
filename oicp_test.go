// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package evroamingsim_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cB-Sigma/evroaming-sim/sim/oicpsim"
	"github.com/cB-Sigma/evroaming/oicp"
)

const (
	cpoOp    = "DE*ABC"
	cpoEVSE  = "DE*ABC*E1001"
	empID    = "DE-DCB"
	cardUID  = "1234ABCD"
	contract = "DE-DCB-C12345678-X"
)

// oicpRig starts oicpsim and returns it with an oicp.Client holding the
// CPO's client certificate.
func oicpRig(t *testing.T) (*oicpsim.Sim, *oicp.Client, *oicpsim.PKI) {
	t.Helper()
	pki, err := oicpsim.NewPKI()
	if err != nil {
		t.Fatal(err)
	}
	s, err := oicpsim.Start(oicpsim.Config{PKI: pki, Cards: map[string]oicpsim.Card{
		cardUID: {ProviderID: empID}, contract: {ProviderID: empID}, "DEADBEEF": {ProviderID: empID, Status: oicp.NotAuthorized, Code: oicp.CodeEvcoLocked}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	pair, err := pki.Pair(cpoOp)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12,
		RootCAs: pki.Pool(), Certificates: []tls.Certificate{pair}}}}
	return s, &oicp.Client{BaseURL: s.URL, HTTP: hc}, pki
}

func record() oicp.EvseDataRecord {
	return oicp.EvseDataRecord{EvseID: cpoEVSE, ChargingStationNames: []oicp.InfoText{{Lang: "en", Value: "Demo"}},
		Address:                        oicp.Address{Country: "DEU", City: "Berlin", Street: "Demo Street", PostalCode: "10115", HouseNum: "1"},
		GeoCoordinates:                 oicp.GeoCoordinates{DecimalDegree: &oicp.DecimalDegree{Longitude: "13.404954", Latitude: "52.520008"}},
		Plugs:                          []string{"Type 2 Outlet"},
		ChargingFacilities:             []oicp.ChargingFacility{{PowerType: oicp.PowerAC3Phase, Power: 22}},
		CalibrationLawDataAvailability: oicp.CalibrationNotAvailable, AuthenticationModes: []string{"NFC RFID Classic", "REMOTE"},
		PaymentOptions: []string{"Contract"}, ValueAddedServices: []string{"None"}, Accessibility: oicp.AccessibilityFree,
		HotlinePhoneNumber: "+4930123456", IsOpen24Hours: true, IsHubjectCompatible: true, DynamicInfoAvailable: oicp.DynamicInfoAuto,
		ChargingPoolID: "DE*ABC*P1001", EnergySource: []oicp.EnergySource{{Energy: "Solar"}}}
}

// Every CPO-to-Hubject operation of oicp.Client is taken by the
// simulator, which checks it and keeps what it was told.
func TestOICPEveryCPOOperation(t *testing.T) {
	s, c, _ := oicpRig(t)
	ctx := context.Background()
	if _, err := c.PushEvseData(ctx, oicp.PushEvseData{ActionType: oicp.ActionFullLoad,
		OperatorEvseData: oicp.OperatorEvseData{OperatorID: cpoOp, OperatorName: "Demo CPO", EvseDataRecord: []oicp.EvseDataRecord{record()}}}); err != nil {
		t.Fatalf("PushEvseData: %v", err)
	}
	if s.EVSE(cpoEVSE) == nil || s.EVSE(cpoEVSE)["ChargingPoolID"] != "DE*ABC*P1001" {
		t.Errorf("EVSE %v", s.EVSE(cpoEVSE))
	}
	if _, err := c.PushEvseStatus(ctx, oicp.PushEvseStatus{ActionType: oicp.ActionUpdate,
		OperatorEvseStatus: oicp.OperatorEvseStatus{OperatorID: cpoOp, EvseStatusRecord: []oicp.EvseStatusRecord{{EvseID: cpoEVSE, EvseStatus: oicp.EvseOccupied}}}}); err != nil {
		t.Fatalf("PushEvseStatus: %v", err)
	}
	if s.Status(cpoEVSE) != oicp.EvseOccupied {
		t.Errorf("status %q", s.Status(cpoEVSE))
	}
	if _, err := c.PushPricingProductData(ctx, oicp.PushPricingProductData{ActionType: oicp.ActionFullLoad, PricingProductData: oicp.PricingProductData{
		OperatorID: cpoOp, ProviderID: empID, PricingDefaultPrice: 0.39, PricingDefaultPriceCurrency: "EUR", PricingDefaultReferenceUnit: oicp.UnitKWh,
		PricingProductDataRecords: []oicp.PricingProductDataRecord{{ProductID: "AC 1", ReferenceUnit: oicp.UnitKWh, ProductPriceCurrency: "EUR",
			PricePerReferenceUnit: 0.39, MaximumProductChargingPower: 22, IsValid24hours: true,
			ProductAvailabilityTimes: []oicp.AvailabilityTime{{Periods: []oicp.Period{{Begin: "00:00", End: "23:59"}}, On: "Everyday"}}}}}}); err != nil {
		t.Fatalf("PushPricingProductData: %v", err)
	}
	if len(s.Products(empID)) != 1 {
		t.Errorf("products %v", s.Products(empID))
	}
	if _, err := c.PushEVSEPricing(ctx, cpoOp, oicp.PushEVSEPricing{ActionType: oicp.ActionFullLoad,
		EVSEPricing: []oicp.EVSEPricing{{EvseID: cpoEVSE, ProviderID: empID, EvseIDProductList: []string{"AC 1"}}}}); err != nil {
		t.Fatalf("PushEVSEPricing: %v", err)
	}
	if len(s.EVSEPricing()) != 1 {
		t.Errorf("EVSE pricing %v", s.EVSEPricing())
	}
	card := oicp.Identification{RFIDMifareFamilyIdentification: &oicp.UIDIdentification{UID: cardUID}}
	a, err := c.AuthorizeStart(ctx, oicp.AuthorizeStart{OperatorID: cpoOp, EvseID: cpoEVSE, Identification: card, CPOPartnerSessionID: "cpo-1"})
	if err != nil || a.AuthorizationStatus != oicp.Authorized || a.SessionID == "" || a.ProviderID != empID {
		t.Fatalf("AuthorizeStart %+v %v", a, err)
	}
	locked, err := c.AuthorizeStart(ctx, oicp.AuthorizeStart{OperatorID: cpoOp, EvseID: cpoEVSE,
		Identification: oicp.Identification{RFIDMifareFamilyIdentification: &oicp.UIDIdentification{UID: "DEADBEEF"}}})
	if err != nil || locked.AuthorizationStatus != oicp.NotAuthorized || locked.StatusCode.Code != oicp.CodeEvcoLocked {
		t.Errorf("locked card %+v %v", locked, err)
	}
	stop, err := c.AuthorizeStop(ctx, oicp.AuthorizeStop{SessionID: a.SessionID, OperatorID: cpoOp, EvseID: cpoEVSE, Identification: card})
	if err != nil || stop.AuthorizationStatus != oicp.Authorized {
		t.Errorf("AuthorizeStop %+v %v", stop, err)
	}
	for _, n := range []oicp.ChargingNotification{
		{Type: oicp.NotificationStart, SessionID: a.SessionID, EvseID: cpoEVSE, ChargingStart: "2026-10-11T08:30:00Z", OperatorID: cpoOp},
		{Type: oicp.NotificationProgress, SessionID: a.SessionID, EvseID: cpoEVSE, ChargingStart: "2026-10-11T08:30:00Z", EventOccurred: "2026-10-11T09:00:00Z", OperatorID: cpoOp},
		{Type: oicp.NotificationEnd, SessionID: a.SessionID, EvseID: cpoEVSE, ChargingEnd: "2026-10-11T09:45:00Z", OperatorID: cpoOp},
		{Type: oicp.NotificationError, SessionID: a.SessionID, EvseID: cpoEVSE, ErrorType: oicp.ErrorConnector, OperatorID: cpoOp},
	} {
		if _, err := c.SendChargingNotification(ctx, n); err != nil {
			t.Errorf("notification %s: %v", n.Type, err)
		}
	}
	if len(s.Notifications()) != 4 {
		t.Errorf("notifications %d", len(s.Notifications()))
	}
	energy := 12.5
	if _, err := c.SendChargeDetailRecord(ctx, cpoOp, oicp.ChargeDetailRecord{SessionID: a.SessionID, EvseID: cpoEVSE, Identification: card,
		ChargingStart: "2026-10-11T08:30:00Z", ChargingEnd: "2026-10-11T09:45:00Z", SessionStart: "2026-10-11T08:29:00Z", SessionEnd: "2026-10-11T09:46:00Z",
		ConsumedEnergy: energy, MeterValueStart: new(float64),
		SignedMeteringValues: []oicp.SignedMeteringValue{{SignedMeteringValue: "c2ln", MeteringStatus: oicp.MeteringStart}}}); err != nil {
		t.Fatalf("CDR: %v", err)
	}
	if len(s.CDRs()) != 1 {
		t.Errorf("CDRs %d", len(s.CDRs()))
	}
}

// Faults: refusals per operation, HTTP errors, a platform that is down
// or slow, and a client certificate of another operator.
func TestOICPFaults(t *testing.T) {
	s, c, pki := oicpRig(t)
	ctx := context.Background()
	status := oicp.PushEvseStatus{ActionType: oicp.ActionUpdate,
		OperatorEvseStatus: oicp.OperatorEvseStatus{OperatorID: cpoOp, EvseStatusRecord: []oicp.EvseStatusRecord{{EvseID: cpoEVSE, EvseStatus: oicp.EvseAvailable}}}}
	var he *oicp.HubjectError

	s.Refuse(oicpsim.OpEvseStatus, oicp.CodeDataError)
	if _, err := c.PushEvseStatus(ctx, status); !errors.As(err, &he) || he.Code != oicp.CodeDataError || oicp.Retryable(err) {
		t.Errorf("refused status: %v", err)
	}
	s.Refuse(oicpsim.OpEvseStatus, "")
	s.Refuse(oicpsim.OpAuthStart, oicp.CodeNoValidContract)
	a, err := c.AuthorizeStart(ctx, oicp.AuthorizeStart{OperatorID: cpoOp, Identification: oicp.Identification{RFIDMifareFamilyIdentification: &oicp.UIDIdentification{UID: cardUID}}})
	if err != nil || a.AuthorizationStatus != oicp.NotAuthorized || a.StatusCode.Code != oicp.CodeNoValidContract {
		t.Errorf("refused authorisation %+v %v", a, err)
	}
	s.Refuse(oicpsim.OpAuthStart, "")

	s.SetHTTPStatus(http.StatusTooManyRequests)
	if _, err := c.PushEvseStatus(ctx, status); !errors.As(err, &he) || he.HTTP != http.StatusTooManyRequests || !oicp.Retryable(err) {
		t.Errorf("429: %v", err)
	}
	s.SetHTTPStatus(0)

	s.SetDown(true)
	if _, err := c.PushEvseStatus(ctx, status); !errors.As(err, &he) || he.HTTP != http.StatusServiceUnavailable || !oicp.Retryable(err) {
		t.Errorf("down: %v", err)
	}
	s.SetDown(false)

	s.RefuseCDRs(oicp.CodeSessionInvalid)
	if _, err := c.SendChargeDetailRecord(ctx, cpoOp, oicp.ChargeDetailRecord{SessionID: "b2688855-7f00-0002-6d8e-48d883f6abb6", EvseID: cpoEVSE,
		Identification: oicp.Identification{RemoteIdentification: &oicp.EvcoIdentification{EvcoID: contract}},
		ChargingStart:  "2026-10-11T08:30:00Z", ChargingEnd: "2026-10-11T09:45:00Z", SessionStart: "2026-10-11T08:30:00Z", SessionEnd: "2026-10-11T09:45:00Z"}); !errors.As(err, &he) || he.Code != oicp.CodeSessionInvalid {
		t.Errorf("refused CDR: %v", err)
	}
	s.RefuseCDRs("")

	s.SetDelay(300 * time.Millisecond)
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = c.PushEvseStatus(short, status)
	cancel()
	if err == nil || !oicp.Retryable(err) {
		t.Errorf("slow platform: %v", err)
	}
	s.SetDelay(0)

	other, err := pki.Pair("DE*XYZ")
	if err != nil {
		t.Fatal(err)
	}
	c2 := &oicp.Client{BaseURL: s.URL, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12,
		RootCAs: pki.Pool(), Certificates: []tls.Certificate{other}}}}}
	if _, err := c2.PushEvseStatus(ctx, status); !errors.As(err, &he) || he.Code != oicp.CodeUnauthorized {
		t.Errorf("another operator's certificate: %v", err)
	}
}

// The simulator's remote start, remote stop and reservation start and
// stop reach a CPO served by oicp.Handler over mutual TLS.
func TestOICPHubjectToCPO(t *testing.T) {
	s, _, pki := oicpRig(t)
	var mu sync.Mutex
	var got []string
	h := &oicp.Handler{
		RemoteStart: func(_ context.Context, m oicp.RemoteStart) oicp.Acknowledgement {
			mu.Lock()
			got = append(got, "start "+m.EvseID+" "+m.Identification.Token())
			mu.Unlock()
			return oicp.NewAck(true, oicp.CodeSuccess, "", "")
		},
		RemoteStop: func(_ context.Context, m oicp.RemoteStop) oicp.Acknowledgement {
			mu.Lock()
			got = append(got, "stop "+m.SessionID)
			mu.Unlock()
			return oicp.NewAck(false, oicp.CodeSessionInvalid, "unknown session", "")
		},
		ReservationStart: func(_ context.Context, m oicp.RemoteReservationStart) oicp.Acknowledgement {
			mu.Lock()
			got = append(got, "reserve "+m.EvseID)
			mu.Unlock()
			if m.Duration != 15 {
				return oicp.NewAck(false, oicp.CodeDataError, "duration", "")
			}
			return oicp.NewAck(true, oicp.CodeSuccess, "", "")
		},
	}
	srv := httptest.NewUnstartedServer(h)
	pair, err := pki.Pair("cpo.example")
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: pki.Pool(), ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	defer srv.Close()
	ctx := context.Background()

	a, sid, err := s.RemoteStart(ctx, srv.URL, empID, cpoEVSE, contract)
	if err != nil || a["Result"] != true || a["SessionID"] != sid {
		t.Errorf("remote start %v %s %v", a, sid, err)
	}
	a, err = s.RemoteStop(ctx, srv.URL, empID, sid, cpoEVSE)
	if err != nil || a["Result"] != false {
		t.Errorf("remote stop %v %v", a, err)
	}
	a, rid, err := s.ReserveStart(ctx, srv.URL, empID, cpoEVSE, contract, 15)
	if err != nil || a["Result"] != true {
		t.Errorf("reservation start %v %v", a, err)
	}
	a, err = s.ReserveStop(ctx, srv.URL, empID, rid, cpoEVSE)
	sc, _ := a["StatusCode"].(map[string]any)
	if err != nil || a["Result"] != false || sc["Code"] != oicp.CodeServiceUnavailable {
		t.Errorf("reservation stop without a callback %v %v", a, err)
	}
	if rs := s.Reservations(); len(rs) != 1 || !rs[0].Stopped || rs[0].Minutes != 15 {
		t.Errorf("reservations %+v", rs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "start "+cpoEVSE+" "+contract {
		t.Errorf("calls %v", got)
	}
}
