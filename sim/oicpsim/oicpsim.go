// Package oicpsim is a Hubject-like OICP 2.3 platform for tests, built
// from Hubject's public description of the CPO side (github.com/hubject/oicp,
// OICP-2.3/OICP 2.3 CPO; github.com/hubject/oicp-cpo-2.3-api-doc) and
// independent of the oicp package: it serves the operations a CPO calls
// (EVSE data and status push, authorize start and stop, charge detail
// records, pricing and charging notifications) over mutual TLS, checks
// that the OperatorID in the path is the one in the caller's client
// certificate (Communication paradigms, Security), checks the messages'
// required fields and identifier forms, answers with the documented JSON
// shapes and status codes, and sends remote starts and stops to a CPO.
package oicpsim

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Card is an EMP's card or contract as Hubject knows it.
type Card struct {
	ProviderID string
	// Status is Authorized (default) or NotAuthorized; Code the status
	// code for NotAuthorized (default 102).
	Status string
	Code   string
}

// Config configures the simulator.
type Config struct {
	// PKI issues the simulator's server and client certificates and
	// checks the CPO's.
	PKI *PKI
	// Cards by UID or EvcoID.
	Cards map[string]Card
	// CDRCode, when set, makes Hubject refuse charge detail records with
	// this status code (Result false).
	CDRCode string
	// Down answers every request with HTTP 503.
	Down bool
	// Listen is the address to serve on (default 127.0.0.1:0, a free
	// loopback port).
	Listen string
}

// Received is a request a CPO made.
type Received struct {
	Path     string
	Operator string // the client certificate's common name
	Body     map[string]any
}

// Session is a session Hubject opened.
type Session struct {
	ID, ProviderID, EvseID, Token string
	Remote, Stopped, CDR          bool
}

// Sim is a running simulator.
type Sim struct {
	cfg    Config
	srv    *http.Server
	ln     net.Listener
	URL    string // base URL, ending in /api/oicp
	client *http.Client

	mu            sync.Mutex
	evses         map[string]map[string]any // EvseID → record
	statuses      map[string]string         // EvseID → status
	sessions      map[string]*Session
	cdrs          []map[string]any
	notifications []map[string]any
	products      map[string][]map[string]any // ProviderID → records
	evsePricing   []map[string]any
	received      []Received
	refuseCDR     string
	down          bool
	refuse        map[string]string // operation → status code
	httpStatus    int
	delay         time.Duration
	reservations  map[string]*Reservation
}

// Operation names for Refuse: the last path elements of the CPO
// operations.
const (
	OpEvseData      = "data-records"
	OpEvseStatus    = "status-records"
	OpAuthStart     = "authorize/start"
	OpAuthStop      = "authorize/stop"
	OpCDR           = "charge-detail-record"
	OpPricing       = "pricing-products"
	OpEVSEPricing   = "evse-pricing"
	OpNotifications = "charging-notifications"
)

// Refuse makes Hubject refuse an operation (Op...) with a status code:
// Result false, or NotAuthorized for the authorisations. "" accepts it
// again.
func (s *Sim) Refuse(op, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse == nil {
		s.refuse = map[string]string{}
	}
	if code == "" {
		delete(s.refuse, op)
	} else {
		s.refuse[op] = code
	}
}

// SetHTTPStatus makes Hubject answer every request with this HTTP status
// and an acknowledgement with status code 001 (0 answers normally); 429
// and 500 are the usual faults.
func (s *Sim) SetHTTPStatus(code int) { s.mu.Lock(); s.httpStatus = code; s.mu.Unlock() }

// SetDelay makes Hubject wait d before answering (a slow platform, for
// client timeouts).
func (s *Sim) SetDelay(d time.Duration) { s.mu.Lock(); s.delay = d; s.mu.Unlock() }

// Reservation is a reservation Hubject asked a CPO for.
type Reservation struct {
	SessionID, ProviderID, EvseID, Token string
	Minutes                              int
	Stopped                              bool
}

// ReserveStart sends eRoamingAuthorizeRemoteReservationStart to a CPO for
// minutes and returns the CPO's acknowledgement and the session ID.
func (s *Sim) ReserveStart(ctx context.Context, cpoURL, providerID, evseID, evcoID string, minutes int) (map[string]any, string, error) {
	sid := guid()
	s.mu.Lock()
	if s.reservations == nil {
		s.reservations = map[string]*Reservation{}
	}
	s.reservations[sid] = &Reservation{SessionID: sid, ProviderID: providerID, EvseID: evseID, Token: evcoID, Minutes: minutes}
	s.mu.Unlock()
	body := map[string]any{"SessionID": sid, "ProviderID": providerID, "EvseID": evseID, "EMPPartnerSessionID": "EMP-" + sid[:8],
		"PartnerProductID": "Reservation", "Duration": minutes,
		"Identification": map[string]any{"RemoteIdentification": map[string]any{"EvcoID": evcoID}}}
	a, err := s.call(ctx, strings.TrimRight(cpoURL, "/")+"/api/oicp/reservation/v11/providers/"+providerID+"/reservation-start-request", body)
	return a, sid, err
}

// ReserveStop sends eRoamingAuthorizeRemoteReservationStop for a
// reservation.
func (s *Sim) ReserveStop(ctx context.Context, cpoURL, providerID, sessionID, evseID string) (map[string]any, error) {
	s.mu.Lock()
	if r := s.reservations[sessionID]; r != nil {
		r.Stopped = true
	}
	s.mu.Unlock()
	body := map[string]any{"SessionID": sessionID, "ProviderID": providerID, "EvseID": evseID}
	return s.call(ctx, strings.TrimRight(cpoURL, "/")+"/api/oicp/reservation/v11/providers/"+providerID+"/reservation-stop-request", body)
}

// Reservations lists the reservations asked for.
func (s *Sim) Reservations() []Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Reservation
	for _, r := range s.reservations {
		out = append(out, *r)
	}
	return out
}

// Start runs a simulator on 127.0.0.1.
func Start(cfg Config) (*Sim, error) {
	if cfg.PKI == nil {
		return nil, fmt.Errorf("oicpsim needs a PKI")
	}
	srvPair, err := cfg.PKI.Pair("service-qa.hubject.test")
	if err != nil {
		return nil, err
	}
	cliPair, err := cfg.PKI.Pair("Hubject")
	if err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	s := &Sim{cfg: cfg, ln: ln, evses: map[string]map[string]any{}, statuses: map[string]string{}, sessions: map[string]*Session{},
		products: map[string][]map[string]any{}, refuseCDR: cfg.CDRCode, down: cfg.Down}
	tcfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{srvPair}, ClientCAs: cfg.PKI.Pool(), ClientAuth: tls.RequireAndVerifyClientCert}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serve)}
	go func() { _ = s.srv.Serve(tls.NewListener(ln, tcfg)) }()
	s.URL = "https://" + ln.Addr().String() + "/api/oicp"
	s.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.PKI.Pool(), Certificates: []tls.Certificate{cliPair}}}}
	return s, nil
}

// Close stops the simulator.
func (s *Sim) Close() { _ = s.srv.Close() }

// SetDown makes Hubject unavailable (HTTP 503) or available again.
func (s *Sim) SetDown(v bool) { s.mu.Lock(); s.down = v; s.mu.Unlock() }

// RefuseCDRs makes Hubject refuse charge detail records with code ("" accepts).
func (s *Sim) RefuseCDRs(code string) { s.mu.Lock(); s.refuseCDR = code; s.mu.Unlock() }

var (
	operatorIDRe = regexp.MustCompile(`^(([A-Za-z]{2}\*?[A-Za-z0-9]{3})|(\+?[0-9]{1,3}\*[0-9]{3}))$`)
	evseIDRe     = regexp.MustCompile(`^(([A-Za-z]{2}\*?[A-Za-z0-9]{3}\*?E[A-Za-z0-9\*]{1,30})|(\+?[0-9]{1,3}\*[0-9]{3}\*[0-9\*]{1,32}))$`)
	opPath       = regexp.MustCompile(`^/api/oicp/(evsepush/v23|evsepush/v21|charging/v21|cdrmgmt/v22|dynamicpricing/v10)/operators/([^/]+)/(data-records|status-records|authorize/start|authorize/stop|charge-detail-record|pricing-products|evse-pricing)$`)
)

func norm(s string) string { return strings.ToUpper(strings.NewReplacer("*", "", "-", "").Replace(s)) }

func ack(result bool, code, desc string) map[string]any {
	return map[string]any{"Result": result, "StatusCode": map[string]any{"Code": code, "Description": desc}}
}

func (s *Sim) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func guid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-4" + h[13:16] + "-a" + h[17:20] + "-" + h[20:]
}

func (s *Sim) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	down, status, delay := s.down, s.httpStatus, s.delay
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if down {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	if status != 0 {
		s.reply(w, status, ack(false, "001", "Hubject system error"))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "only POST", http.StatusMethodNotAllowed)
		return
	}
	cn := ""
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cn = r.TLS.PeerCertificates[0].Subject.CommonName
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var body map[string]any
	if err := json.Unmarshal(b, &body); err != nil {
		s.reply(w, http.StatusBadRequest, ack(false, "022", "the body is not JSON"))
		return
	}
	s.mu.Lock()
	s.received = append(s.received, Received{Path: r.URL.Path, Operator: cn, Body: body})
	s.mu.Unlock()
	if r.URL.Path == "/api/oicp/notificationmgmt/v11/charging-notifications" {
		if op, _ := body["OperatorID"].(string); norm(op) != norm(cn) {
			s.reply(w, http.StatusForbidden, ack(false, "017", "Unauthorized Access"))
			return
		}
		if s.refused(w, OpNotifications) {
			return
		}
		s.notification(w, body)
		return
	}
	m := opPath.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	op := m[2]
	if !operatorIDRe.MatchString(op) || norm(op) != norm(cn) {
		// The OperatorID must be the one in the client certificate.
		s.reply(w, http.StatusForbidden, ack(false, "017", "Unauthorized Access"))
		return
	}
	if s.refused(w, m[3]) {
		return
	}
	switch m[3] {
	case "data-records":
		s.evseData(w, op, body)
	case "status-records":
		s.evseStatus(w, op, body)
	case "authorize/start":
		s.authorizeStart(w, op, body)
	case "authorize/stop":
		s.authorizeStop(w, body)
	case "charge-detail-record":
		s.cdr(w, body)
	case "pricing-products":
		s.pricing(w, op, body)
	case "evse-pricing":
		s.evsePricingPush(w, body)
	}
}

// refused answers a refusal set with Refuse for op; it reports whether
// it did.
func (s *Sim) refused(w http.ResponseWriter, op string) bool {
	s.mu.Lock()
	code := s.refuse[op]
	s.mu.Unlock()
	if code == "" {
		return false
	}
	if op == OpAuthStart || op == OpAuthStop {
		s.reply(w, http.StatusOK, map[string]any{"AuthorizationStatus": "NotAuthorized", "StatusCode": map[string]any{"Code": code, "Description": "refused"}})
	} else {
		s.reply(w, http.StatusOK, ack(false, code, "refused"))
	}
	return true
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

var evseRequired = []string{"EvseID", "ChargingStationNames", "Address", "GeoCoordinates", "Plugs", "ChargingFacilities", "RenewableEnergy",
	"CalibrationLawDataAvailability", "AuthenticationModes", "PaymentOptions", "ValueAddedServices", "Accessibility", "HotlinePhoneNumber",
	"IsOpen24Hours", "IsHubjectCompatible", "DynamicInfoAvailable"}

var actionTypes = map[string]bool{"fullLoad": true, "update": true, "insert": true, "delete": true}

func (s *Sim) evseData(w http.ResponseWriter, op string, body map[string]any) {
	at := str(body, "ActionType")
	oed, _ := body["OperatorEvseData"].(map[string]any)
	if !actionTypes[at] || oed == nil || str(oed, "OperatorID") == "" || str(oed, "OperatorName") == "" {
		s.reply(w, http.StatusOK, ack(false, "022", "ActionType and OperatorEvseData with OperatorID and OperatorName are needed"))
		return
	}
	recs, _ := oed["EvseDataRecord"].([]any)
	parsed := map[string]map[string]any{}
	for _, x := range recs {
		rec, _ := x.(map[string]any)
		for _, f := range evseRequired {
			if _, ok := rec[f]; !ok {
				s.reply(w, http.StatusOK, ack(false, "022", "EvseDataRecord without "+f))
				return
			}
		}
		id := str(rec, "EvseID")
		if !evseIDRe.MatchString(id) || !strings.HasPrefix(norm(id), norm(op)) {
			s.reply(w, http.StatusOK, ack(false, "018", "Inconsistent EvseID "+id))
			return
		}
		if a, _ := rec["Address"].(map[string]any); len(str(a, "Country")) != 3 {
			s.reply(w, http.StatusOK, ack(false, "022", "Address.Country is an ISO 3166-1 alpha-3 code"))
			return
		}
		parsed[norm(id)] = rec
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch at {
	case "fullLoad":
		s.evses = parsed
	case "insert", "update":
		for k, v := range parsed {
			s.evses[k] = v
		}
	case "delete":
		for k := range parsed {
			delete(s.evses, k)
		}
	}
	s.reply(w, http.StatusOK, ack(true, "000", "Success"))
}

var statuses = map[string]bool{"Available": true, "Reserved": true, "Occupied": true, "OutOfService": true, "EvseNotFound": true, "Unknown": true}

func (s *Sim) evseStatus(w http.ResponseWriter, op string, body map[string]any) {
	at := str(body, "ActionType")
	oes, _ := body["OperatorEvseStatus"].(map[string]any)
	if !actionTypes[at] || oes == nil || str(oes, "OperatorID") == "" {
		s.reply(w, http.StatusOK, ack(false, "022", "ActionType and OperatorEvseStatus with OperatorID are needed"))
		return
	}
	recs, _ := oes["EvseStatusRecord"].([]any)
	parsed := map[string]string{}
	for _, x := range recs {
		rec, _ := x.(map[string]any)
		id, st := str(rec, "EvseID"), str(rec, "EvseStatus")
		if !evseIDRe.MatchString(id) || !strings.HasPrefix(norm(id), norm(op)) {
			s.reply(w, http.StatusOK, ack(false, "018", "Inconsistent EvseID "+id))
			return
		}
		if !statuses[st] {
			s.reply(w, http.StatusOK, ack(false, "022", "unknown EvseStatus "+st))
			return
		}
		parsed[norm(id)] = st
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if at == "fullLoad" {
		s.statuses = map[string]string{}
	}
	for k, v := range parsed {
		if at == "delete" {
			delete(s.statuses, k)
		} else {
			s.statuses[k] = v
		}
	}
	s.reply(w, http.StatusOK, ack(true, "000", "Success"))
}

// identification reads IdentificationType: the identifier and whether a
// member is filled.
func identification(body map[string]any) (string, string, bool) {
	id, _ := body["Identification"].(map[string]any)
	for _, k := range []string{"RFIDMifareFamilyIdentification", "RFIDIdentification", "RemoteIdentification", "PlugAndChargeIdentification", "QRCodeIdentification"} {
		if m, ok := id[k].(map[string]any); ok {
			if v := str(m, "UID"); v != "" {
				return k, v, true
			}
			if v := str(m, "EvcoID"); v != "" {
				return k, v, true
			}
		}
	}
	return "", "", false
}

func (s *Sim) authorizeStart(w http.ResponseWriter, op string, body map[string]any) {
	kind, tok, ok := identification(body)
	if !ok || str(body, "OperatorID") == "" {
		s.reply(w, http.StatusOK, map[string]any{"AuthorizationStatus": "NotAuthorized", "StatusCode": map[string]any{"Code": "022", "Description": "OperatorID and Identification are needed"}})
		return
	}
	if kind == "RFIDIdentification" {
		// Not for authorisation (IdentificationType).
		s.reply(w, http.StatusOK, map[string]any{"AuthorizationStatus": "NotAuthorized", "StatusCode": map[string]any{"Code": "022", "Description": "RFIDIdentification is not used for authorisation"}})
		return
	}
	if e := str(body, "EvseID"); e != "" && !strings.HasPrefix(norm(e), norm(op)) {
		s.reply(w, http.StatusOK, map[string]any{"AuthorizationStatus": "NotAuthorized", "StatusCode": map[string]any{"Code": "018", "Description": "Inconsistent EvseID"}})
		return
	}
	card, known := s.cfg.Cards[tok]
	if !known {
		s.reply(w, http.StatusOK, map[string]any{"AuthorizationStatus": "NotAuthorized", "StatusCode": map[string]any{"Code": "102", "Description": "RFID Authentication failed - invalid UID"}})
		return
	}
	resp := map[string]any{"ProviderID": card.ProviderID, "CPOPartnerSessionID": str(body, "CPOPartnerSessionID")}
	if card.Status == "NotAuthorized" {
		code := card.Code
		if code == "" {
			code = "102"
		}
		resp["AuthorizationStatus"], resp["StatusCode"] = "NotAuthorized", map[string]any{"Code": code}
		s.reply(w, http.StatusOK, resp)
		return
	}
	sid := guid()
	s.mu.Lock()
	s.sessions[sid] = &Session{ID: sid, ProviderID: card.ProviderID, EvseID: str(body, "EvseID"), Token: tok}
	s.mu.Unlock()
	resp["SessionID"], resp["AuthorizationStatus"] = sid, "Authorized"
	resp["StatusCode"] = map[string]any{"Code": "000", "Description": "Success"}
	resp["AuthorizationStopIdentifications"] = []any{body["Identification"]}
	s.reply(w, http.StatusOK, resp)
}

func (s *Sim) authorizeStop(w http.ResponseWriter, body map[string]any) {
	_, _, ok := identification(body)
	sid := str(body, "SessionID")
	s.mu.Lock()
	ss := s.sessions[sid]
	if ss != nil && ok {
		ss.Stopped = true
	}
	s.mu.Unlock()
	if ss == nil || !ok {
		s.reply(w, http.StatusOK, map[string]any{"SessionID": sid, "AuthorizationStatus": "NotAuthorized", "StatusCode": map[string]any{"Code": "400", "Description": "Session is invalid"}})
		return
	}
	s.reply(w, http.StatusOK, map[string]any{"SessionID": sid, "ProviderID": ss.ProviderID, "AuthorizationStatus": "Authorized",
		"StatusCode": map[string]any{"Code": "000", "Description": "Success"}})
}

var cdrRequired = []string{"SessionID", "EvseID", "Identification", "ChargingStart", "ChargingEnd", "SessionStart", "SessionEnd", "ConsumedEnergy"}

var tsRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?([+-]\d{2}:\d{2}|Z)$`)

func (s *Sim) cdr(w http.ResponseWriter, body map[string]any) {
	for _, f := range cdrRequired {
		if _, ok := body[f]; !ok {
			s.reply(w, http.StatusOK, ack(false, "022", "the record has no "+f))
			return
		}
	}
	for _, f := range []string{"ChargingStart", "ChargingEnd", "SessionStart", "SessionEnd"} {
		if !tsRe.MatchString(str(body, f)) {
			s.reply(w, http.StatusOK, ack(false, "022", f+" is not a date and time with an offset"))
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuseCDR != "" {
		s.reply(w, http.StatusOK, ack(false, s.refuseCDR, "refused"))
		return
	}
	ss := s.sessions[str(body, "SessionID")]
	if ss == nil {
		s.reply(w, http.StatusOK, ack(false, "400", "Session is invalid"))
		return
	}
	if ss.CDR {
		s.reply(w, http.StatusOK, ack(false, "022", "a record was already sent for the session"))
		return
	}
	ss.CDR = true
	s.cdrs = append(s.cdrs, body)
	a := ack(true, "000", "Success")
	a["SessionID"] = ss.ID
	s.reply(w, http.StatusOK, a)
}

func (s *Sim) notification(w http.ResponseWriter, body map[string]any) {
	typ := str(body, "Type")
	need := map[string][]string{"Start": {"ChargingStart"}, "Progress": {"ChargingStart", "EventOccurred"}, "End": {"ChargingEnd"}, "Error": {"ErrorType"}}
	fields, ok := need[typ]
	if !ok || str(body, "SessionID") == "" || str(body, "EvseID") == "" {
		s.reply(w, http.StatusOK, ack(false, "022", "Type, SessionID and EvseID are needed"))
		return
	}
	for _, f := range fields {
		if _, ok := body[f]; !ok {
			s.reply(w, http.StatusOK, ack(false, "022", typ+" needs "+f))
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[str(body, "SessionID")] == nil {
		s.reply(w, http.StatusOK, ack(false, "400", "Session is invalid"))
		return
	}
	s.notifications = append(s.notifications, body)
	s.reply(w, http.StatusOK, ack(true, "000", "Success"))
}

func (s *Sim) pricing(w http.ResponseWriter, op string, body map[string]any) {
	ppd, _ := body["PricingProductData"].(map[string]any)
	if !actionTypes[str(body, "ActionType")] || ppd == nil {
		s.reply(w, http.StatusOK, ack(false, "022", "ActionType and PricingProductData are needed"))
		return
	}
	for _, f := range []string{"OperatorID", "ProviderID", "PricingDefaultPrice", "PricingDefaultPriceCurrency", "PricingDefaultReferenceUnit", "PricingProductDataRecords"} {
		if _, ok := ppd[f]; !ok {
			s.reply(w, http.StatusOK, ack(false, "022", "PricingProductData has no "+f))
			return
		}
	}
	recs, _ := ppd["PricingProductDataRecords"].([]any)
	var list []map[string]any
	for _, x := range recs {
		rec, _ := x.(map[string]any)
		for _, f := range []string{"ProductID", "ReferenceUnit", "ProductPriceCurrency", "PricePerReferenceUnit", "MaximumProductChargingPower", "IsValid24hours", "ProductAvailabilityTimes"} {
			if _, ok := rec[f]; !ok {
				s.reply(w, http.StatusOK, ack(false, "022", "a pricing product has no "+f))
				return
			}
		}
		list = append(list, rec)
	}
	s.mu.Lock()
	s.products[str(ppd, "ProviderID")] = list
	s.mu.Unlock()
	s.reply(w, http.StatusOK, ack(true, "000", "Success"))
}

func (s *Sim) evsePricingPush(w http.ResponseWriter, body map[string]any) {
	list, _ := body["EVSEPricing"].([]any)
	if !actionTypes[str(body, "ActionType")] {
		s.reply(w, http.StatusOK, ack(false, "022", "ActionType is needed"))
		return
	}
	var out []map[string]any
	for _, x := range list {
		m, _ := x.(map[string]any)
		if str(m, "EvseID") == "" || str(m, "ProviderID") == "" || m["EvseIDProductList"] == nil {
			s.reply(w, http.StatusOK, ack(false, "022", "EVSEPricing needs EvseID, ProviderID and EvseIDProductList"))
			return
		}
		out = append(out, m)
	}
	s.mu.Lock()
	s.evsePricing = out
	s.mu.Unlock()
	s.reply(w, http.StatusOK, ack(true, "000", "Success"))
}

// RemoteStart sends eRoamingAuthorizeRemoteStart to a CPO's endpoint (the
// URL entered in the portal), as an EMP's app start reaches Hubject: it
// opens a session and returns the CPO's acknowledgement.
func (s *Sim) RemoteStart(ctx context.Context, cpoURL, providerID, evseID, evcoID string) (map[string]any, string, error) {
	sid := guid()
	s.mu.Lock()
	s.sessions[sid] = &Session{ID: sid, ProviderID: providerID, EvseID: evseID, Token: evcoID, Remote: true}
	s.mu.Unlock()
	body := map[string]any{"SessionID": sid, "ProviderID": providerID, "EvseID": evseID, "EMPPartnerSessionID": "EMP-" + sid[:8],
		"Identification": map[string]any{"RemoteIdentification": map[string]any{"EvcoID": evcoID}}}
	a, err := s.call(ctx, strings.TrimRight(cpoURL, "/")+"/api/oicp/charging/v21/providers/"+providerID+"/authorize-remote/start", body)
	return a, sid, err
}

// RemoteStop sends eRoamingAuthorizeRemoteStop for a session.
func (s *Sim) RemoteStop(ctx context.Context, cpoURL, providerID, sessionID, evseID string) (map[string]any, error) {
	body := map[string]any{"SessionID": sessionID, "ProviderID": providerID, "EvseID": evseID}
	return s.call(ctx, strings.TrimRight(cpoURL, "/")+"/api/oicp/charging/v21/providers/"+providerID+"/authorize-remote/stop", body)
}

func (s *Sim) call(ctx context.Context, u string, body any) (map[string]any, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the CPO answered HTTP %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// EVSE returns an EVSE's data record as last pushed.
func (s *Sim) EVSE(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evses[norm(id)]
}

// EVSECount is how many EVSE records Hubject holds.
func (s *Sim) EVSECount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.evses) }

// Status returns an EVSE's status as last pushed.
func (s *Sim) Status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statuses[norm(id)]
}

// Session returns a session.
func (s *Sim) Session(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := s.sessions[id]; x != nil {
		c := *x
		return &c
	}
	return nil
}

// CDRs lists the charge detail records taken.
func (s *Sim) CDRs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.cdrs...)
}

// Notifications lists the charging notifications taken.
func (s *Sim) Notifications() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.notifications...)
}

// Products lists the pricing products pushed for a provider.
func (s *Sim) Products(provider string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.products[provider]
}

// EVSEPricing lists the EVSE pricing last pushed.
func (s *Sim) EVSEPricing() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.evsePricing...)
}

// Received lists the CPO's requests.
func (s *Sim) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}
