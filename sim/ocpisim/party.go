// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package ocpisim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cB-Sigma/evroaming/ocpi"
)

// Party is a simulated OCPI 2.2.1 platform of any role, built on
// ocpi.Router: it serves every module in both interface roles (the
// Sender interfaces from the objects it owns, the Receiver interfaces
// into a store of what partners push), registers with a partner or lets
// one register, answers commands and charging profile requests with
// results POSTed to the response_url, and has fault switches per module.
// Unlike Sim, which was written without the library, Party uses the
// library's router and client helpers; it exists so that every operation
// of the library has a counterpart to talk to.
type Party struct {
	cfg    PartyConfig
	ln     net.Listener
	srv    *http.Server
	router *ocpi.Router
	// URL is the base URL; versions at URL+"/versions".
	URL string

	mu        sync.Mutex
	tokens    map[string]bool // credentials tokens partners use with us
	peerToken string          // the token we use with the registered partner
	peerEPs   []ocpi.Endpoint
	peerCreds *ocpi.Credentials
	own       map[string]map[string]any // module → id → object we own
	stored    map[string]map[string]any // module → key → object pushed to us
	results   map[string]any            // response_url path → result
	prefs     map[string]ocpi.ChargingPreferences
	profiles  map[string]any // session → SetChargingProfile or ActiveChargingProfile
	faults    map[string]Fault
	cmdResult string
	calls     []string
}

// PartyConfig describes a Party.
type PartyConfig struct {
	// Self is the party; Role is CPO, EMSP, HUB, SCSP...
	Self ocpi.Party
	Role string
	// TokenA is the registration token a partner registers with.
	TokenA string
	// PageLimit is the page size of its lists (default 2, to make
	// partners page).
	PageLimit int
	// Listen is the address (default 127.0.0.1:0).
	Listen string
	// PublicHost, when set, is the host name partners reach the party
	// at (its port is the listening port); for example
	// host.docker.internal for a partner in a container.
	PublicHost string
	// TrailingSlash publishes endpoint URLs ending in "/"
	// (ocpi.Router.TrailingSlash).
	TrailingSlash bool
}

// Fault makes a module's interface answer with an HTTP status and OCPI
// status code, after Delay. Code 0 with HTTP 0 is no fault.
type Fault struct {
	HTTP  int
	Code  int
	Delay time.Duration
}

// NewParty starts a Party.
func NewParty(cfg PartyConfig) (*Party, error) {
	if cfg.PageLimit <= 0 {
		cfg.PageLimit = 2
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.Role == "" {
		cfg.Role = ocpi.RoleCPO
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	base := "http://" + ln.Addr().String() + "/ocpi"
	if cfg.PublicHost != "" {
		base = fmt.Sprintf("http://%s:%d/ocpi", cfg.PublicHost, ln.Addr().(*net.TCPAddr).Port)
	}
	p := &Party{cfg: cfg, ln: ln, URL: base, tokens: map[string]bool{},
		own: map[string]map[string]any{}, stored: map[string]map[string]any{}, results: map[string]any{},
		prefs: map[string]ocpi.ChargingPreferences{}, profiles: map[string]any{}, faults: map[string]Fault{}, cmdResult: ocpi.CmdResultAccepted}
	m := &memory{p}
	p.router = &ocpi.Router{BaseURL: p.URL, Self: cfg.Self, Authenticate: p.authenticate, PageLimit: cfg.PageLimit, TrailingSlash: cfg.TrailingSlash,
		Credentials: m, LocationsSender: m, LocationsReceiver: m, SessionsSender: m, SessionsReceiver: m,
		CDRsSender: m, CDRsReceiver: m, TariffsSender: m, TariffsReceiver: m, TokensSender: m, TokensReceiver: m,
		CommandsReceiver: m, CommandsSender: m, ChargingProfilesReceiver: m, ChargingProfilesSender: m,
		HubClientInfoReceiver: m, HubClientInfoSender: m}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// Close stops the party.
func (p *Party) Close() { _ = p.srv.Close() }

// Router is the party's ocpi.Router (its endpoints, URLs).
func (p *Party) Router() *ocpi.Router { return p.router }

// VersionsURL is the party's versions endpoint.
func (p *Party) VersionsURL() string { return p.router.VersionsURL() }

// SetFault sets (or, with the zero Fault, clears) a fault on a module's
// interfaces ("locations", "commands", ..., or "credentials").
func (p *Party) SetFault(module string, f Fault) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f == (Fault{}) {
		delete(p.faults, module)
	} else {
		p.faults[module] = f
	}
}

// SetCommandResult sets the CommandResult value the party POSTs after
// accepting a command (default ACCEPTED); "" posts none (a timeout).
func (p *Party) SetCommandResult(r string) { p.mu.Lock(); p.cmdResult = r; p.mu.Unlock() }

// Own sets an object the party owns and serves from its Sender
// interfaces: module is "locations", "sessions", "cdrs", "tariffs",
// "tokens" or "hubclientinfo"; v is the ocpi object.
func (p *Party) Own(module, id string, v any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.own[module] == nil {
		p.own[module] = map[string]any{}
	}
	p.own[module][id] = v
}

// Stored returns what partners pushed to a Receiver interface, by key
// ("CC/PID/id[/evse[/connector]]"), as JSON-shaped values.
func (p *Party) Stored(module string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{}
	for k, v := range p.stored[module] {
		out[k] = v
	}
	return out
}

// Results returns the results POSTed to the party's response URLs, by
// path below its endpoint.
func (p *Party) Results() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{}
	for k, v := range p.results {
		out[k] = v
	}
	return out
}

// Profile is the charging profile request or active profile last seen
// for a session.
func (p *Party) Profile(session string) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.profiles[session]
}

// Preferences are the charging preferences set for a session.
func (p *Party) Preferences(session string) (ocpi.ChargingPreferences, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.prefs[session]
	return c, ok
}

// Calls lists "METHOD path" of the requests the party served.
func (p *Party) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// Token is the credentials token the registered partner uses with the
// party ("" before registration).
func (p *Party) Token() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peerTokenFor()
}

// PeerToken is the credentials token the party uses with its partner.
func (p *Party) PeerToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peerToken
}

// PeerEndpoints are the registered partner's endpoints.
func (p *Party) PeerEndpoints() []ocpi.Endpoint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ocpi.Endpoint(nil), p.peerEPs...)
}

// Doer is a Doer that calls the registered partner with its token.
func (p *Party) Doer() ocpi.Doer {
	c := &ocpi.Client{From: p.cfg.Self, HTTP: &http.Client{Timeout: 10 * time.Second}}
	return func(ctx context.Context, r ocpi.Request) (*ocpi.Reply, error) {
		tok := r.Token
		if tok == "" {
			p.mu.Lock()
			tok = p.peerToken
			p.mu.Unlock()
		}
		return c.Send(ctx, r, tok, false)
	}
}

// PeerEndpoint is the partner's URL for a module and interface role.
func (p *Party) PeerEndpoint(module, role string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.peerEPs {
		if e.Identifier == module && e.Role == role {
			return e.URL
		}
	}
	return ""
}

func (p *Party) ourCredentials(token string) ocpi.Credentials {
	return ocpi.Credentials{Token: token, URL: p.VersionsURL(), Roles: []ocpi.CredentialsRole{{Role: p.cfg.Role,
		BusinessDetails: ocpi.BusinessDetails{Name: "Simulated " + p.cfg.Role}, PartyID: p.cfg.Self.PartyID, CountryCode: p.cfg.Self.CountryCode}}}
}

// Register registers with a partner from its versions URL and token A
// (the credentials handshake, 7.1.1): it reads the partner's endpoints,
// POSTs credentials with a new token for the partner, and keeps the
// token the partner returns.
func (p *Party) Register(ctx context.Context, versionsURL, tokenA string) error {
	do := p.Doer()
	_, eps, err := ocpi.FetchEndpoints(ctx, do, versionsURL, tokenA, nil)
	if err != nil {
		return err
	}
	credURL := ocpi.EndpointURL(eps, ocpi.ModCredentials)
	if credURL == "" {
		return errors.New("the partner lists no credentials endpoint")
	}
	tokB := newToken()
	p.mu.Lock()
	p.tokens[tokB] = true
	p.mu.Unlock()
	theirs, err := ocpi.RegisterCredentials(ctx, do, credURL, tokenA, p.ourCredentials(tokB))
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.peerToken, p.peerCreds = theirs.Token, &theirs
	p.mu.Unlock()
	// The endpoints again, now with the partner's token C.
	_, eps, err = ocpi.FetchEndpoints(ctx, do, theirs.URL, theirs.Token, nil)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.peerEPs = eps
	p.mu.Unlock()
	return nil
}

func (p *Party) authenticate(_ *http.Request, token string) (any, bool, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tokens[token] {
		return "partner", false, true
	}
	if token != "" && token == p.cfg.TokenA {
		return "registering", true, true
	}
	return nil, false, false
}

func (p *Party) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.calls = append(p.calls, r.Method+" "+r.URL.Path)
	var f Fault
	for mod, ff := range p.faults {
		if strings.Contains(r.URL.Path, "/"+mod) {
			f = ff
		}
	}
	p.mu.Unlock()
	if f.Delay > 0 {
		select {
		case <-time.After(f.Delay):
		case <-r.Context().Done():
			return
		}
	}
	if f.HTTP != 0 || f.Code != 0 {
		h, c := f.HTTP, f.Code
		if h == 0 {
			h = http.StatusOK
		}
		if c == 0 {
			c = ocpi.StatusServerError
		}
		ocpi.SetRequestIDs(w, r)
		ocpi.WriteReply(w, h, c, "simulated fault", nil, time.Now())
		return
	}
	p.router.ServeHTTP(w, r)
}

// memory implements every module interface over the Party's maps.
type memory struct{ p *Party }

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func fromMap(m any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func key(owner ocpi.Party, ids ...string) string {
	return strings.Join(append([]string{ocpi.Upper(owner.CountryCode), ocpi.Upper(owner.PartyID)}, ids...), "/")
}

func (m *memory) items(module string) []ocpi.Item {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	var ids []string
	for id := range m.p.own[module] {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []ocpi.Item
	for _, id := range ids {
		v := m.p.own[module][id]
		lu, _ := toMap(v)["last_updated"].(string)
		t, _ := ocpi.ParseTime(lu)
		out = append(out, ocpi.Item{Updated: t, Value: v})
	}
	return out
}

func (m *memory) put(module, k string, v any) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	if m.p.stored[module] == nil {
		m.p.stored[module] = map[string]any{}
	}
	m.p.stored[module][k] = toMap(v)
}

func (m *memory) get(module, k string) (map[string]any, error) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	v, ok := m.p.stored[module][k].(map[string]any)
	if !ok {
		return nil, ocpi.ErrNotFound
	}
	return v, nil
}

func (m *memory) patch(module, k string, fields map[string]any) error {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	v, ok := m.p.stored[module][k].(map[string]any)
	if !ok {
		return ocpi.ErrNotFound
	}
	for f, x := range fields {
		v[f] = x
	}
	return nil
}

// Credentials.

func (m *memory) GetCredentials(_ context.Context, c *ocpi.Call) (ocpi.Credentials, error) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	return m.p.ourCredentials(m.p.peerTokenFor()), nil
}

// peerTokenFor is the token a partner uses with us (the latest).
func (p *Party) peerTokenFor() string {
	for t := range p.tokens {
		return t
	}
	return ""
}

func (m *memory) Register(ctx context.Context, c *ocpi.Call, theirs ocpi.Credentials) (ocpi.Credentials, error) {
	if c.Partner != "registering" {
		return ocpi.Credentials{}, &ocpi.CallError{HTTP: http.StatusMethodNotAllowed, Code: ocpi.StatusClientError, Message: "already registered: use PUT"}
	}
	// Read the registering party's endpoints with its token B.
	m.p.mu.Lock()
	m.p.peerToken = theirs.Token
	m.p.mu.Unlock()
	_, eps, err := ocpi.FetchEndpoints(ctx, m.p.Doer(), theirs.URL, theirs.Token, nil)
	if err != nil {
		return ocpi.Credentials{}, &ocpi.CallError{HTTP: http.StatusOK, Code: ocpi.StatusClientAPIError, Message: err.Error()}
	}
	tokC := newToken()
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.peerEPs, m.p.peerCreds = eps, &theirs
	m.p.tokens = map[string]bool{tokC: true}
	return m.p.ourCredentials(tokC), nil
}

func (m *memory) UpdateCredentials(ctx context.Context, c *ocpi.Call, theirs ocpi.Credentials) (ocpi.Credentials, error) {
	if c.Partner == "registering" {
		return ocpi.Credentials{}, &ocpi.CallError{HTTP: http.StatusMethodNotAllowed, Code: ocpi.StatusClientError, Message: "not registered: use POST"}
	}
	tokC := newToken()
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.peerToken, m.p.peerCreds = theirs.Token, &theirs
	m.p.tokens = map[string]bool{tokC: true}
	return m.p.ourCredentials(tokC), nil
}

func (m *memory) Unregister(_ context.Context, c *ocpi.Call) error {
	if c.Partner == "registering" {
		return &ocpi.CallError{HTTP: http.StatusMethodNotAllowed, Code: ocpi.StatusClientError, Message: "not registered"}
	}
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.tokens, m.p.peerToken, m.p.peerEPs, m.p.peerCreds = map[string]bool{}, "", nil, nil
	return nil
}

// Locations.

func (m *memory) Locations(context.Context, *ocpi.Call) ([]ocpi.Item, error) {
	return m.items(ocpi.ModLocations), nil
}

func (m *memory) Location(_ context.Context, _ *ocpi.Call, ids ...string) (any, error) {
	m.p.mu.Lock()
	v, ok := m.p.own[ocpi.ModLocations][ids[0]]
	m.p.mu.Unlock()
	if !ok {
		return nil, ocpi.ErrNotFound
	}
	var loc ocpi.Location
	if err := fromMap(v, &loc); err != nil {
		return nil, err
	}
	if len(ids) == 1 {
		return loc, nil
	}
	for _, e := range loc.EVSEs {
		if e.UID != ids[1] {
			continue
		}
		if len(ids) == 2 {
			return e, nil
		}
		for _, cn := range e.Connectors {
			if cn.ID == ids[2] {
				return cn, nil
			}
		}
	}
	return nil, ocpi.ErrNotFound
}

func (m *memory) GetLocation(_ context.Context, _ *ocpi.Call, owner ocpi.Party, ids ...string) (any, error) {
	return m.get(ocpi.ModLocations, key(owner, ids...))
}

func (m *memory) PutLocation(_ context.Context, _ *ocpi.Call, owner ocpi.Party, v any, ids ...string) error {
	m.put(ocpi.ModLocations, key(owner, ids...), v)
	return nil
}

func (m *memory) PatchLocation(_ context.Context, _ *ocpi.Call, owner ocpi.Party, fields map[string]any, ids ...string) error {
	return m.patch(ocpi.ModLocations, key(owner, ids...), fields)
}

// Sessions.

func (m *memory) Sessions(context.Context, *ocpi.Call) ([]ocpi.Item, error) {
	return m.items(ocpi.ModSessions), nil
}

func (m *memory) SetChargingPreferences(_ context.Context, _ *ocpi.Call, id string, c ocpi.ChargingPreferences) (string, error) {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	if _, ok := m.p.own[ocpi.ModSessions][id]; !ok {
		return "", ocpi.ErrNotFound
	}
	if c.ProfileType == ocpi.ProfileGreen {
		return ocpi.PrefsProfileTypeNotSupported, nil
	}
	m.p.prefs[id] = c
	return ocpi.PrefsAccepted, nil
}

func (m *memory) GetSession(_ context.Context, _ *ocpi.Call, owner ocpi.Party, id string) (ocpi.Session, error) {
	var s ocpi.Session
	v, err := m.get(ocpi.ModSessions, key(owner, id))
	if err == nil {
		err = fromMap(v, &s)
	}
	return s, err
}

func (m *memory) PutSession(_ context.Context, _ *ocpi.Call, owner ocpi.Party, s ocpi.Session) error {
	m.put(ocpi.ModSessions, key(owner, s.ID), s)
	return nil
}

func (m *memory) PatchSession(_ context.Context, _ *ocpi.Call, owner ocpi.Party, id string, fields map[string]any) error {
	return m.patch(ocpi.ModSessions, key(owner, id), fields)
}

// CDRs.

func (m *memory) CDRs(context.Context, *ocpi.Call) ([]ocpi.Item, error) {
	return m.items(ocpi.ModCDRs), nil
}

func (m *memory) PostCDR(_ context.Context, _ *ocpi.Call, c ocpi.CDR) error {
	k := key(ocpi.Party{CountryCode: c.CountryCode, PartyID: c.PartyID}, c.ID)
	if _, err := m.get(ocpi.ModCDRs, k); err == nil {
		return &ocpi.CallError{HTTP: http.StatusOK, Code: ocpi.StatusInvalidParams, Message: "a CDR with this id exists"}
	}
	m.put(ocpi.ModCDRs, k, c)
	return nil
}

func (m *memory) GetCDR(_ context.Context, _ *ocpi.Call, owner ocpi.Party, id string) (ocpi.CDR, error) {
	var c ocpi.CDR
	v, err := m.get(ocpi.ModCDRs, key(owner, id))
	if err == nil {
		err = fromMap(v, &c)
	}
	return c, err
}

// Tariffs.

func (m *memory) Tariffs(context.Context, *ocpi.Call) ([]ocpi.Item, error) {
	return m.items(ocpi.ModTariffs), nil
}

func (m *memory) GetTariff(_ context.Context, _ *ocpi.Call, owner ocpi.Party, id string) (ocpi.Tariff, error) {
	var t ocpi.Tariff
	v, err := m.get(ocpi.ModTariffs, key(owner, id))
	if err == nil {
		err = fromMap(v, &t)
	}
	return t, err
}

func (m *memory) PutTariff(_ context.Context, _ *ocpi.Call, owner ocpi.Party, t ocpi.Tariff) error {
	m.put(ocpi.ModTariffs, key(owner, t.ID), t)
	return nil
}

func (m *memory) DeleteTariff(_ context.Context, _ *ocpi.Call, owner ocpi.Party, id string) error {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	k := key(owner, id)
	if _, ok := m.p.stored[ocpi.ModTariffs][k]; !ok {
		return ocpi.ErrNotFound
	}
	delete(m.p.stored[ocpi.ModTariffs], k)
	return nil
}

// Tokens.

func (m *memory) Tokens(context.Context, *ocpi.Call) ([]ocpi.Item, error) {
	return m.items(ocpi.ModTokens), nil
}

func (m *memory) Authorize(_ context.Context, _ *ocpi.Call, uid, typ string, loc *ocpi.LocationReferences) (ocpi.AuthorizationInfo, error) {
	m.p.mu.Lock()
	v, ok := m.p.own[ocpi.ModTokens][uid]
	m.p.mu.Unlock()
	if !ok {
		return ocpi.AuthorizationInfo{}, ocpi.ErrNotFound
	}
	var t ocpi.Token
	if err := fromMap(v, &t); err != nil {
		return ocpi.AuthorizationInfo{}, err
	}
	if t.Type != typ {
		return ocpi.AuthorizationInfo{}, ocpi.ErrNotFound
	}
	ai := ocpi.AuthorizationInfo{Allowed: ocpi.AllowedAllowed, Token: t, Location: loc, AuthorizationReference: "auth-" + uid}
	if !t.Valid {
		ai.Allowed = ocpi.AllowedBlocked
		ai.AuthorizationReference = ""
	}
	return ai, nil
}

func (m *memory) GetToken(_ context.Context, _ *ocpi.Call, owner ocpi.Party, uid, typ string) (ocpi.Token, error) {
	var t ocpi.Token
	v, err := m.get(ocpi.ModTokens, key(owner, uid, typ))
	if err == nil {
		err = fromMap(v, &t)
	}
	return t, err
}

func (m *memory) PutToken(_ context.Context, _ *ocpi.Call, owner ocpi.Party, t ocpi.Token) error {
	typ := t.Type
	if typ == "" {
		typ = ocpi.TokenRFID
	}
	m.put(ocpi.ModTokens, key(owner, t.UID, typ), t)
	return nil
}

func (m *memory) PatchToken(_ context.Context, _ *ocpi.Call, owner ocpi.Party, uid, typ string, fields map[string]any) error {
	return m.patch(ocpi.ModTokens, key(owner, uid, typ), fields)
}

// Commands: the command is accepted, and its result POSTed to the
// response_url shortly after (unless SetCommandResult("")).

func (m *memory) Command(_ context.Context, _ *ocpi.Call, command string, body any) (ocpi.CommandResponse, error) {
	var respURL string
	switch b := body.(type) {
	case *ocpi.StartSession:
		respURL = b.ResponseURL
		if b.LocationID == "" {
			return ocpi.CommandResponse{}, &ocpi.CallError{HTTP: http.StatusOK, Code: ocpi.StatusInvalidParams, Message: "location_id is needed"}
		}
		m.p.mu.Lock()
		_, known := m.p.own[ocpi.ModLocations][b.LocationID]
		m.p.mu.Unlock()
		if !known {
			return ocpi.CommandResponse{}, &ocpi.CallError{HTTP: http.StatusNotFound, Code: ocpi.StatusUnknownLocation, Message: "unknown location"}
		}
	case *ocpi.StopSession:
		respURL = b.ResponseURL
		m.p.mu.Lock()
		_, known := m.p.own[ocpi.ModSessions][b.SessionID]
		m.p.mu.Unlock()
		if !known {
			return ocpi.CommandResponse{Result: ocpi.CmdRespUnknownSession, Timeout: 30}, nil
		}
	case *ocpi.ReserveNow:
		respURL = b.ResponseURL
	case *ocpi.CancelReservation:
		respURL = b.ResponseURL
	case *ocpi.UnlockConnector:
		respURL = b.ResponseURL
	}
	m.p.mu.Lock()
	res := m.p.cmdResult
	m.p.mu.Unlock()
	if res != "" && respURL != "" {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = ocpi.PostCommandResult(context.Background(), m.p.Doer(), respURL, nil, ocpi.CommandResult{Result: res})
		}()
	}
	return ocpi.CommandResponse{Result: ocpi.CmdRespAccepted, Timeout: 30}, nil
}

func (m *memory) CommandResult(_ context.Context, _ *ocpi.Call, path []string, r ocpi.CommandResult) error {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.results[strings.Join(path, "/")] = r
	return nil
}

// Charging profiles: requests are accepted and answered with a result
// POSTed to the response_url.

func (m *memory) profileSession(id string) bool {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	_, ok := m.p.own[ocpi.ModSessions][id]
	return ok
}

func (m *memory) later(u string, v any) {
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = ocpi.PostChargingProfileResult(context.Background(), m.p.Doer(), u, nil, v)
	}()
}

func (m *memory) GetActiveProfile(_ context.Context, _ *ocpi.Call, id string, duration int, u string) (ocpi.ChargingProfileResponse, error) {
	if !m.profileSession(id) {
		return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespUnknownSession, Timeout: 30}, nil
	}
	m.p.mu.Lock()
	var cp ocpi.ChargingProfile
	switch v := m.p.profiles[id].(type) {
	case ocpi.SetChargingProfile:
		cp = v.ChargingProfile
	case ocpi.ActiveChargingProfile:
		cp = v.ChargingProfile
	default:
		cp = ocpi.ChargingProfile{ChargingRateUnit: ocpi.RateAmperes, ChargingProfilePeriod: []ocpi.ChargingProfilePeriod{{StartPeriod: 0, Limit: 32}}}
	}
	m.p.mu.Unlock()
	cp.Duration = &duration
	m.later(u, ocpi.ActiveChargingProfileResult{Result: ocpi.ProfileResultAccepted,
		Profile: &ocpi.ActiveChargingProfile{StartDateTime: ocpi.FormatTime(time.Now()), ChargingProfile: cp}})
	return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespAccepted, Timeout: 30}, nil
}

func (m *memory) SetProfile(_ context.Context, _ *ocpi.Call, id string, sp ocpi.SetChargingProfile) (ocpi.ChargingProfileResponse, error) {
	if !m.profileSession(id) {
		return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespUnknownSession, Timeout: 30}, nil
	}
	if sp.ChargingProfile.ChargingRateUnit != ocpi.RateWatts && sp.ChargingProfile.ChargingRateUnit != ocpi.RateAmperes {
		return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespRejected, Timeout: 30}, nil
	}
	m.p.mu.Lock()
	m.p.profiles[id] = sp
	m.p.mu.Unlock()
	m.later(sp.ResponseURL, ocpi.ChargingProfileResult{Result: ocpi.ProfileResultAccepted})
	return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespAccepted, Timeout: 30}, nil
}

func (m *memory) ClearProfile(_ context.Context, _ *ocpi.Call, id, u string) (ocpi.ChargingProfileResponse, error) {
	if !m.profileSession(id) {
		return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespUnknownSession, Timeout: 30}, nil
	}
	m.p.mu.Lock()
	delete(m.p.profiles, id)
	m.p.mu.Unlock()
	m.later(u, ocpi.ClearProfileResult{Result: ocpi.ProfileResultAccepted})
	return ocpi.ChargingProfileResponse{Result: ocpi.ProfileRespAccepted, Timeout: 30}, nil
}

func (m *memory) ProfileResult(_ context.Context, _ *ocpi.Call, path []string, raw json.RawMessage) error {
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.results[strings.Join(path, "/")] = v
	return nil
}

func (m *memory) PutActiveProfile(_ context.Context, _ *ocpi.Call, id string, ap ocpi.ActiveChargingProfile) error {
	m.p.mu.Lock()
	defer m.p.mu.Unlock()
	m.p.profiles[id] = ap
	return nil
}

// Hub client info.

func (m *memory) GetClientInfo(_ context.Context, _ *ocpi.Call, party ocpi.Party) (ocpi.ClientInfo, error) {
	var ci ocpi.ClientInfo
	v, err := m.get(ocpi.ModHubClientInfo, key(party))
	if err == nil {
		err = fromMap(v, &ci)
	}
	return ci, err
}

func (m *memory) PutClientInfo(_ context.Context, _ *ocpi.Call, ci ocpi.ClientInfo) error {
	m.put(ocpi.ModHubClientInfo, key(ocpi.Party{CountryCode: ci.CountryCode, PartyID: ci.PartyID}), ci)
	return nil
}

func (m *memory) ClientInfos(context.Context, *ocpi.Call) ([]ocpi.Item, error) {
	return m.items(ocpi.ModHubClientInfo), nil
}

// String describes the party for logs.
func (p *Party) String() string {
	return fmt.Sprintf("%s %s at %s", p.cfg.Role, p.cfg.Self.Key(), p.URL)
}
