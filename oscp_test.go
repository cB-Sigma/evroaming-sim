// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package evroamingsim_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/cB-Sigma/evroaming-sim/sim/oscpsim"
	"github.com/cB-Sigma/evroaming/oscp"
)

func oscpTime(d time.Duration) string {
	return time.Now().Add(d).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// oscpSample is a valid message for an endpoint; back is the base URL a
// Register points at.
func oscpSample(ep, back string) any {
	iv := 60
	blocks := []oscp.ForecastedBlock{{Capacity: 10, Phase: "ALL", Unit: oscp.UnitKW, StartTime: oscpTime(time.Hour), EndTime: oscpTime(2 * time.Hour)}}
	switch ep {
	case oscp.EPRegister:
		return oscp.Register{Token: "token-back", VersionURL: []oscp.VersionURL{{Version: oscp.Version, BaseURL: back}}}
	case oscp.EPHandshake, oscp.EPHandshakeAcknowledge:
		return oscp.Handshake{RequiredBehaviour: &oscp.RequiredBehaviour{HeartbeatInterval: &iv, MeasurementConfiguration: []string{oscp.MeasurementContinuous}}}
	case oscp.EPHeartbeat:
		return oscp.Heartbeat{OfflineModeAt: oscpTime(5 * time.Minute)}
	case oscp.EPUpdateGroupCapacityForecast, oscp.EPAdjustGroupCapacityForecast:
		return oscp.GroupCapacityForecast{GroupID: "group-1", Type: oscp.TypeConsumption, ForecastedBlocks: blocks}
	case oscp.EPGroupCapacityComplianceError:
		return oscp.GroupCapacityComplianceError{Message: "the group drew more than its capacity", ForecastedBlocks: blocks}
	case oscp.EPUpdateGroupMeasurements:
		return oscp.UpdateGroupMeasurements{GroupID: "group-1", Measurements: []oscp.EnergyMeasurement{{Value: 5, Phase: "ALL", Unit: oscp.UnitKWH,
			Direction: "IMPORT", EnergyType: "TOTAL", MeasureTime: oscpTime(-time.Minute), InitialMeasureTime: oscpTime(-time.Hour)}}}
	case oscp.EPUpdateAssetMeasurements:
		return oscp.UpdateAssetMeasurements{GroupID: "group-1", Measurements: []oscp.AssetMeasurement{{AssetID: "asset-1", AssetCategory: "CHARGING",
			EnergyMeasurement: &oscp.EnergyMeasurement{Value: 2, Phase: "ALL", Unit: oscp.UnitKWH, Direction: "IMPORT", MeasureTime: oscpTime(-time.Minute)}},
			{AssetID: "asset-2", AssetCategory: "STORAGE",
				InstantaneousMeasurement: &oscp.InstantaneousMeasurement{Value: 7, Phase: "ALL", Unit: oscp.UnitKW, MeasureTime: oscpTime(-time.Minute)}}}}
	}
	return nil
}

func oscpHeaders(tok string) http.Header {
	h := http.Header{}
	h.Set(oscp.HeaderAuthorization, "Token "+tok)
	h.Set(oscp.HeaderRequestID, oscp.NewRequestID())
	h.Set(oscp.HeaderCorrelationID, oscp.NewRequestID())
	return h
}

// Every message of 4.2, in every direction CanSend allows, posted with
// oscp.Post and validated by the receiving simulated party against the
// OCA schemas; segmented, and under the variant spelling of
// handshake_acknowledge.
func TestOSCPEveryMessage(t *testing.T) {
	ctx := context.Background()
	n := 0
	for _, e := range oscp.Endpoints() {
		for owner, senders := range e.Owners {
			for _, from := range senders {
				if !oscp.CanSend(from, owner, e.Name) {
					t.Errorf("CanSend(%s, %s, %s) is false", from, owner, e.Name)
				}
				recv, err := oscpsim.New(owner, "token-in")
				if err != nil {
					t.Fatal(err)
				}
				back, err := oscpsim.New(from, "token-back")
				if err != nil {
					t.Fatal(err)
				}
				body, _ := json.Marshal(oscpSample(e.Name, back.BaseURL()))
				if err := oscp.Validate(e.Schema, body); err != nil {
					t.Errorf("%s sample: %v", e.Name, err)
				}
				if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+e.Name, oscpHeaders("token-in"), body); err != nil {
					t.Errorf("%s %s→%s: %v", e.Name, from, owner, err)
				}
				if got := recv.Received(e.Name); len(got) != 1 {
					t.Errorf("%s %s→%s: received %d", e.Name, from, owner, len(got))
				}
				n++
				recv.Close()
				back.Close()
			}
		}
	}
	t.Logf("%d message directions", n)

	// Segments, the variant endpoint spelling, a refused message, faults.
	recv, err := oscpsim.New(oscp.RoleFP, "token-in")
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	body, _ := json.Marshal(oscpSample(oscp.EPUpdateGroupCapacityForecast, ""))
	segs := oscp.Segments(body, 40)
	first := ""
	for i, s := range segs {
		h := oscpHeaders("token-in")
		if i == 0 {
			first = h.Get(oscp.HeaderRequestID)
		}
		h.Set(oscp.HeaderCorrelationID, first)
		h.Set(oscp.HeaderSegmentIndex, itoa(i+1))
		h.Set(oscp.HeaderSegmentCount, itoa(len(segs)))
		if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPUpdateGroupCapacityForecast, h, s); err != nil {
			t.Fatalf("segment %d: %v", i+1, err)
		}
	}
	if len(segs) < 2 || len(recv.Segments(oscp.EPUpdateGroupCapacityForecast)) != len(segs) || len(recv.Received(oscp.EPUpdateGroupCapacityForecast)) != 1 {
		t.Errorf("%d segments, %d received, %d messages", len(segs), len(recv.Segments(oscp.EPUpdateGroupCapacityForecast)), len(recv.Received(oscp.EPUpdateGroupCapacityForecast)))
	}
	ack, _ := json.Marshal(oscpSample(oscp.EPHandshakeAcknowledge, ""))
	if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPHandshakeAcknowledgment, oscpHeaders("token-in"), ack); err != nil || len(recv.Received(oscp.EPHandshakeAcknowledge)) != 1 {
		t.Errorf("variant spelling: %v", err)
	}
	var se *oscp.SendError
	bad := []byte(`{"group_id":"g","type":"SOMETHING","forecasted_blocks":[]}`)
	if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPUpdateGroupCapacityForecast, oscpHeaders("token-in"), bad); !errors.As(err, &se) || se.Status != http.StatusBadRequest || se.Message == "" {
		t.Errorf("invalid message: %v", err)
	}
	if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPHeartbeat, oscpHeaders("wrong"), body); !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Errorf("wrong token: %v", err)
	}
	hb, _ := json.Marshal(oscpSample(oscp.EPHeartbeat, ""))
	recv.Fail(oscp.EPHeartbeat, http.StatusServiceUnavailable)
	if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPHeartbeat, oscpHeaders("token-in"), hb); !errors.As(err, &se) || se.Status != http.StatusServiceUnavailable {
		t.Errorf("fault: %v", err)
	}
	recv.Fail(oscp.EPHeartbeat, 0)
	recv.SetDelay(300 * time.Millisecond)
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	if err := oscp.Post(short, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPHeartbeat, oscpHeaders("token-in"), hb); err == nil {
		t.Error("a slow party did not time out")
	}
	cancel()
	recv.SetDelay(0)
	if err := oscp.Post(ctx, nil, http.MethodPost, recv.BaseURL()+"/"+oscp.EPHeartbeat, oscpHeaders("token-in"), hb); err != nil {
		t.Errorf("after the faults: %v", err)
	}
}

// Register both ways, handshake and acknowledgement: two simulated
// parties come online.
func TestOSCPRegisterHandshake(t *testing.T) {
	cp, err := oscpsim.New(oscp.RoleCP, "token-for-fp")
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	fp, err := oscpsim.New(oscp.RoleFP, "")
	if err != nil {
		t.Fatal(err)
	}
	defer fp.Close()
	if err := fp.Register(cp.BaseURL(), "token-for-fp"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && len(fp.Received(oscp.EPRegister)) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if err := cp.Handshake(); err != nil {
		t.Fatalf("handshake: %v (%s)", err, cp.LastError())
	}
	for i := 0; i < 100 && !cp.Online(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !cp.Online() || !fp.Online() {
		t.Errorf("online cp %v fp %v", cp.Online(), fp.Online())
	}
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
