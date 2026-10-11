// Package ocpisim is an OCPI 2.2.1 partner platform for tests: an
// eMobility service provider, or a hub that relays a charge point
// operator's locations and lists its connected parties. It is written from
// the public OCPI 2.2.1 text (github.com/ocpi/ocpi, tag 2.2.1), not from
// the library, and behaves as a real partner does: it checks the
// Authorization header (Base64, or the token as it is when configured
// so), lets its registration token open only the versions and
// credentials, wraps every answer in the response envelope with OCPI
// status codes, serves its lists in pages with Link, X-Total-Count and
// X-Limit, and receives sessions, CDRs, location updates and command
// results.
package ocpisim

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config describes the simulated platform.
type Config struct {
	// Role is EMSP (default) or HUB.
	Role         string
	CountryCode  string
	PartyID      string
	BusinessName string
	// TokenA is the registration token we issue to the operator, for it
	// to register with us.
	TokenA string
	// RawTokens: send and expect credentials tokens without Base64 (as
	// OCPI 2.1.1 did); a Base64 token is then refused with 401.
	RawTokens bool
	// PageSize is the most objects per page we serve (default 2).
	PageSize int
	// Tokens are our drivers' tokens (Token objects).
	Tokens []map[string]any
	// Authorize answers real-time authorisation by uid: ALLOWED, BLOCKED…;
	// a uid not listed is unknown (404, 2004).
	Authorize map[string]string
	// Locations a hub serves from its locations sender (relayed).
	Locations []map[string]any
	// Clients a hub lists in hubclientinfo.
	Clients []map[string]any
	// Listen is the address to serve on (default 127.0.0.1:0, a free
	// loopback port).
	Listen string
}

// Received is one request the simulator got.
type Received struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

// Sim is a running simulated platform.
type Sim struct {
	cfg Config
	ln  net.Listener
	srv *http.Server
	URL string // base URL; versions at URL+"/versions"

	mu         sync.Mutex
	registered bool
	ourToken   string            // the token the operator uses with us (C, or B when we registered)
	theirToken string            // the token we use with the operator
	endpoints  map[string]string // module/ROLE → URL
	received   []Received
	sessions   map[string]map[string]any
	cdrs       []map[string]any
	results    map[string]map[string]any
	seq        int
}

// Start runs the simulator on a loopback port.
func Start(cfg Config) (*Sim, error) {
	if cfg.Role == "" {
		cfg.Role = "EMSP"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 2
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	s := &Sim{cfg: cfg, ln: ln, URL: "http://" + ln.Addr().String() + "/ocpi", endpoints: map[string]string{},
		sessions: map[string]map[string]any{}, results: map[string]map[string]any{}}
	s.srv = &http.Server{Handler: http.HandlerFunc(s.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Close stops the simulator.
func (s *Sim) Close() { _ = s.srv.Close() }

// VersionsURL is our versions endpoint.
func (s *Sim) VersionsURL() string { return s.URL + "/versions" }

func (s *Sim) roles() []any {
	return []any{map[string]any{"role": s.cfg.Role, "party_id": s.cfg.PartyID, "country_code": s.cfg.CountryCode,
		"business_details": map[string]any{"name": s.cfg.BusinessName}}}
}

func (s *Sim) details() map[string]any {
	b := s.URL + "/2.2.1"
	eps := []any{
		ep("credentials", "SENDER", b+"/credentials"),
		ep("tokens", "SENDER", b+"/tokens"),
		ep("sessions", "RECEIVER", b+"/sessions"),
		ep("cdrs", "RECEIVER", b+"/cdrs"),
		ep("locations", "RECEIVER", b+"/locations"),
		ep("tariffs", "RECEIVER", b+"/tariffs"),
		ep("commands", "SENDER", b+"/commands"),
	}
	if s.cfg.Role == "HUB" {
		eps = append(eps, ep("locations", "SENDER", b+"/hub/locations"), ep("hubclientinfo", "SENDER", b+"/hubclientinfo"))
	}
	return map[string]any{"version": "2.2.1", "endpoints": eps}
}

func ep(id, role, url string) map[string]any {
	return map[string]any{"identifier": id, "role": role, "url": url}
}

func newToken() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Sim) env(w http.ResponseWriter, httpStatus, code int, msg string, data any) {
	m := map[string]any{"status_code": code, "timestamp": time.Now().UTC().Format("2006-01-02T15:04:05Z")}
	if msg != "" {
		m["status_message"] = msg
	}
	if data != nil {
		m["data"] = data
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(m)
}

// token reads the Authorization header.
func (s *Sim) token(r *http.Request) (string, bool) {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Token ")
	if !ok {
		return "", false
	}
	if s.cfg.RawTokens {
		return v, true
	}
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func (s *Sim) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
	w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	path := strings.TrimPrefix(r.URL.Path, "/ocpi")
	s.mu.Lock()
	s.received = append(s.received, Received{Method: r.Method, Path: path, Header: r.Header.Clone(), Body: body})
	ours, registered := s.ourToken, s.registered
	s.mu.Unlock()
	tok, ok := s.token(r)
	isA := ok && !registered && s.cfg.TokenA != "" && tok == s.cfg.TokenA
	if !ok || (tok != ours && !isA) || tok == "" {
		s.env(w, http.StatusUnauthorized, 2000, "unknown credentials token", nil)
		return
	}
	config := path == "/versions" || path == "/2.2.1" || path == "/2.2.1/credentials"
	if isA && !config {
		s.env(w, http.StatusUnauthorized, 2000, "the registration token only opens versions and credentials", nil)
		return
	}
	if !config {
		// Answers of functional modules carry the routing headers back.
		w.Header().Set("OCPI-to-country-code", r.Header.Get("OCPI-from-country-code"))
		w.Header().Set("OCPI-to-party-id", r.Header.Get("OCPI-from-party-id"))
		w.Header().Set("OCPI-from-country-code", s.cfg.CountryCode)
		w.Header().Set("OCPI-from-party-id", s.cfg.PartyID)
	}
	seg := strings.Split(strings.Trim(strings.TrimPrefix(path, "/2.2.1"), "/"), "/")
	switch {
	case path == "/versions":
		s.env(w, 200, 1000, "", []any{map[string]any{"version": "2.2.1", "url": s.URL + "/2.2.1"}})
	case path == "/2.2.1":
		s.env(w, 200, 1000, "", s.details())
	case path == "/2.2.1/credentials":
		s.serveCredentials(w, r, body, isA)
	case seg[0] == "tokens":
		s.serveTokens(w, r, seg[1:])
	case seg[0] == "sessions" && len(seg) == 4 && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		s.mu.Lock()
		cur := s.sessions[seg[3]]
		if cur == nil && r.Method == http.MethodPatch {
			s.mu.Unlock()
			s.env(w, 404, 2000, "unknown session", nil)
			return
		}
		if r.Method == http.MethodPut || cur == nil {
			cur = map[string]any{}
		}
		for k, v := range body {
			cur[k] = v
		}
		s.sessions[seg[3]] = cur
		s.mu.Unlock()
		s.env(w, 200, 1000, "", nil)
	case seg[0] == "cdrs" && r.Method == http.MethodPost:
		s.mu.Lock()
		s.cdrs = append(s.cdrs, body)
		s.mu.Unlock()
		w.Header().Set("Location", s.URL+"/2.2.1/cdrs/"+fmt.Sprint(body["id"]))
		s.env(w, 200, 1000, "", nil)
	case (seg[0] == "locations" || seg[0] == "tariffs") && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		s.env(w, 200, 1000, "", nil)
	case seg[0] == "commands" && r.Method == http.MethodPost && len(seg) == 3:
		s.mu.Lock()
		s.results[seg[2]] = body
		s.mu.Unlock()
		s.env(w, 200, 1000, "", nil)
	case seg[0] == "hub" && len(seg) == 2 && seg[1] == "locations" && r.Method == http.MethodGet && s.cfg.Role == "HUB":
		s.page(w, r, s.cfg.Locations)
	case seg[0] == "hubclientinfo" && r.Method == http.MethodGet && s.cfg.Role == "HUB":
		s.page(w, r, s.cfg.Clients)
	default:
		s.env(w, 404, 2000, "no such endpoint", nil)
	}
}

func (s *Sim) serveCredentials(w http.ResponseWriter, r *http.Request, body map[string]any, isA bool) {
	switch r.Method {
	case http.MethodGet:
		tok, _ := s.token(r)
		s.env(w, 200, 1000, "", map[string]any{"token": tok, "url": s.VersionsURL(), "roles": s.roles()})
	case http.MethodPost, http.MethodPut:
		s.mu.Lock()
		reg := s.registered
		s.mu.Unlock()
		if r.Method == http.MethodPost && reg {
			s.env(w, 405, 2000, "already registered", nil)
			return
		}
		if r.Method == http.MethodPut && !reg {
			s.env(w, 405, 2000, "not registered", nil)
			return
		}
		theirs, _ := body["token"].(string)
		vurl, _ := body["url"].(string)
		if theirs == "" || vurl == "" {
			s.env(w, 400, 2001, "token and url are required", nil)
			return
		}
		s.mu.Lock()
		s.theirToken = theirs
		s.mu.Unlock()
		if err := s.readEndpoints(r.Context(), vurl); err != nil {
			s.env(w, 200, 3001, "cannot use your API: "+err.Error(), nil)
			return
		}
		mine := newToken()
		s.mu.Lock()
		s.ourToken, s.registered = mine, true
		s.mu.Unlock()
		s.env(w, 200, 1000, "", map[string]any{"token": mine, "url": s.VersionsURL(), "roles": s.roles()})
	case http.MethodDelete:
		s.mu.Lock()
		reg := s.registered
		s.registered, s.ourToken, s.theirToken = false, "", ""
		s.mu.Unlock()
		if !reg {
			s.env(w, 405, 2000, "not registered", nil)
			return
		}
		s.env(w, 200, 1000, "", nil)
	default:
		s.env(w, 405, 2000, "method not allowed", nil)
	}
}

func (s *Sim) serveTokens(w http.ResponseWriter, r *http.Request, seg []string) {
	switch {
	case r.Method == http.MethodGet && (len(seg) == 0 || seg[0] == ""):
		s.page(w, r, s.cfg.Tokens)
	case r.Method == http.MethodPost && len(seg) == 2 && seg[1] == "authorize":
		allowed, ok := s.cfg.Authorize[seg[0]]
		if !ok {
			s.env(w, 404, 2004, "unknown token", nil)
			return
		}
		tok := map[string]any{"uid": seg[0], "type": "RFID", "country_code": s.cfg.CountryCode, "party_id": s.cfg.PartyID,
			"contract_id": s.cfg.CountryCode + "-" + s.cfg.PartyID + "-" + seg[0], "issuer": s.cfg.BusinessName, "valid": true,
			"whitelist": "NEVER", "last_updated": "2026-10-01T00:00:00Z"}
		s.env(w, 200, 1000, "", map[string]any{"allowed": allowed, "token": tok, "authorization_reference": "AUTH-" + seg[0]})
	default:
		s.env(w, 405, 2000, "method not allowed", nil)
	}
}

// page serves a list with offset/limit, Link, X-Total-Count and X-Limit.
func (s *Sim) page(w http.ResponseWriter, r *http.Request, all []map[string]any) {
	off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	lim := s.cfg.PageSize
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l < lim {
		lim = l
	}
	end := off + lim
	if end > len(all) {
		end = len(all)
	}
	out := []any{}
	if off < len(all) {
		for _, x := range all[off:end] {
			out = append(out, x)
		}
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(len(all)))
	w.Header().Set("X-Limit", strconv.Itoa(s.cfg.PageSize))
	if end < len(all) {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?offset=%d&limit=%d>; rel="next"`, "http://"+r.Host, r.URL.Path, end, lim))
	}
	s.env(w, 200, 1000, "", out)
}

// call sends a request to the operator.
func (s *Sim) call(ctx context.Context, method, url, token string, body any, to bool) (map[string]any, http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, nil, err
	}
	if s.cfg.RawTokens {
		req.Header.Set("Authorization", "Token "+token)
	} else {
		req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
	}
	s.mu.Lock()
	s.seq++
	n := s.seq
	s.mu.Unlock()
	req.Header.Set("X-Request-ID", fmt.Sprintf("sim-req-%d", n))
	req.Header.Set("X-Correlation-ID", fmt.Sprintf("sim-corr-%d", n))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if to {
		req.Header.Set("OCPI-from-country-code", s.cfg.CountryCode)
		req.Header.Set("OCPI-from-party-id", s.cfg.PartyID)
		req.Header.Set("OCPI-to-country-code", "")
		req.Header.Del("OCPI-to-country-code")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	var env map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, resp.Header, fmt.Errorf("HTTP %d without an OCPI envelope", resp.StatusCode)
	}
	code, _ := env["status_code"].(float64)
	if resp.StatusCode >= 300 || code < 1000 || code > 1999 {
		return env, resp.Header, fmt.Errorf("HTTP %d, OCPI status %v: %v", resp.StatusCode, env["status_code"], env["status_message"])
	}
	return env, resp.Header, nil
}

func (s *Sim) readEndpoints(ctx context.Context, versionsURL string) error {
	s.mu.Lock()
	tok := s.theirToken
	s.mu.Unlock()
	return s.readEndpointsWith(ctx, versionsURL, tok)
}

func (s *Sim) readEndpointsWith(ctx context.Context, versionsURL, tok string) error {
	env, _, err := s.call(ctx, http.MethodGet, versionsURL, tok, nil, false)
	if err != nil {
		return err
	}
	vs, _ := env["data"].([]any)
	durl := ""
	for _, v := range vs {
		m, _ := v.(map[string]any)
		if m["version"] == "2.2.1" {
			durl, _ = m["url"].(string)
		}
	}
	if durl == "" {
		return errors.New("no 2.2.1")
	}
	env, _, err = s.call(ctx, http.MethodGet, durl, tok, nil, false)
	if err != nil {
		return err
	}
	det, _ := env["data"].(map[string]any)
	eps, _ := det["endpoints"].([]any)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints = map[string]string{}
	for _, e := range eps {
		m, _ := e.(map[string]any)
		s.endpoints[fmt.Sprint(m["identifier"])+"/"+fmt.Sprint(m["role"])] = fmt.Sprint(m["url"])
	}
	return nil
}

// Register registers with the operator from its versions URL and the
// token A it gave us (we are the credentials sender).
func (s *Sim) Register(ctx context.Context, versionsURL, tokenA string) error {
	if err := s.readEndpointsWith(ctx, versionsURL, tokenA); err != nil {
		return err
	}
	tokB := newToken()
	s.mu.Lock()
	s.ourToken = tokB
	cred := s.endpoints["credentials/SENDER"]
	if cred == "" {
		cred = s.endpoints["credentials/RECEIVER"]
	}
	s.mu.Unlock()
	env, _, err := s.call(ctx, http.MethodPost, cred, tokenA, map[string]any{"token": tokB, "url": s.VersionsURL(), "roles": s.roles()}, false)
	if err != nil {
		return err
	}
	data, _ := env["data"].(map[string]any)
	c, _ := data["token"].(string)
	if c == "" {
		return errors.New("no token C in the answer")
	}
	s.mu.Lock()
	s.theirToken, s.registered = c, true
	s.mu.Unlock()
	return nil
}

// Unregister sends DELETE to the operator's credentials.
func (s *Sim) Unregister(ctx context.Context) error {
	s.mu.Lock()
	cred, tok := s.endpoints["credentials/SENDER"], s.theirToken
	s.mu.Unlock()
	_, _, err := s.call(ctx, http.MethodDelete, cred, tok, nil, false)
	return err
}

// Registered says whether the credentials exchange finished.
func (s *Sim) Registered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registered && s.theirToken != ""
}

// Endpoint is the operator's endpoint URL for module and role.
func (s *Sim) Endpoint(module, role string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpoints[module+"/"+role]
}

// Do sends a request to an operator endpoint with our credentials and
// routing headers; it returns the envelope and the response headers.
func (s *Sim) Do(ctx context.Context, method, url string, body any) (map[string]any, http.Header, error) {
	s.mu.Lock()
	tok := s.theirToken
	s.mu.Unlock()
	return s.call(ctx, method, url, tok, body, true)
}

// DoWithToken is Do with an explicit token and raw (not Base64) encoding
// choice, for authentication tests.
func (s *Sim) DoWithToken(ctx context.Context, method, url, header string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, nil, err
	}
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var env map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env, nil
}

// TheirToken is the credentials token we use with the operator.
func (s *Sim) TheirToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.theirToken
}

// PushToken PUTs one of our tokens to the operator's tokens receiver.
func (s *Sim) PushToken(ctx context.Context, tok map[string]any) error {
	u := s.Endpoint("tokens", "RECEIVER")
	if u == "" {
		return errors.New("the operator has no tokens receiver")
	}
	typ, _ := tok["type"].(string)
	if typ == "" {
		typ = "RFID"
	}
	_, _, err := s.Do(ctx, http.MethodPut, strings.TrimRight(u, "/")+"/"+s.cfg.CountryCode+"/"+s.cfg.PartyID+"/"+fmt.Sprint(tok["uid"])+"?type="+typ, tok)
	return err
}

// Command POSTs a command with a fresh response_url; it returns the
// CommandResponse and the id the result will be filed under.
func (s *Sim) Command(ctx context.Context, typ string, body map[string]any) (map[string]any, string, error) {
	u := s.Endpoint("commands", "RECEIVER")
	if u == "" {
		return nil, "", errors.New("the operator has no commands receiver")
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("c%d", s.seq)
	s.mu.Unlock()
	body["response_url"] = s.URL + "/2.2.1/commands/" + typ + "/" + id
	env, _, err := s.Do(ctx, http.MethodPost, strings.TrimRight(u, "/")+"/"+typ, body)
	data, _ := env["data"].(map[string]any)
	return data, id, err
}

// Result is the CommandResult filed under id, nil until it arrives.
func (s *Sim) Result(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[id]
}

// Session is a session the operator pushed, nil if none.
func (s *Sim) Session(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.sessions[id]; m != nil {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		return c
	}
	return nil
}

// Sessions lists the pushed sessions' IDs.
func (s *Sim) Sessions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.sessions {
		out = append(out, k)
	}
	return out
}

// CDRs lists the CDRs the operator posted.
func (s *Sim) CDRs() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.cdrs...)
}

// Received lists the requests the simulator got.
func (s *Sim) Received() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}

var linkRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// GetAll reads a list of the operator's sender interface page by page
// with the given page limit, following Link; it returns the objects, the
// pages read and the last X-Total-Count.
func (s *Sim) GetAll(ctx context.Context, module string, limit int) ([]map[string]any, int, int, error) {
	u := s.Endpoint(module, "SENDER")
	if u == "" {
		return nil, 0, 0, fmt.Errorf("the operator has no %s sender", module)
	}
	if limit > 0 {
		u += "?limit=" + strconv.Itoa(limit)
	}
	var out []map[string]any
	pages, total := 0, -1
	for u != "" && pages < 100 {
		env, h, err := s.Do(ctx, http.MethodGet, u, nil)
		if err != nil {
			return out, pages, total, err
		}
		pages++
		list, _ := env["data"].([]any)
		for _, x := range list {
			if m, ok := x.(map[string]any); ok {
				out = append(out, m)
			}
		}
		total, _ = strconv.Atoi(h.Get("X-Total-Count"))
		u = ""
		if m := linkRE.FindStringSubmatch(h.Get("Link")); m != nil {
			u = m[1]
		}
	}
	return out, pages, total, nil
}
