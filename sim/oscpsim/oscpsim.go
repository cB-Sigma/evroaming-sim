// Package oscpsim is an OSCP 2.0 party for tests and development: a
// Capacity Provider, Flexibility Provider or Capacity Optimizer with its
// own HTTP endpoints, written independently of the IPE's state machine. It
// answers registration (registering back with a new token), handshakes
// (acknowledging after its 204) and every message of its role with 204
// after checking the Authorization token, X-Request-ID and the message's
// schema, and records what it received. Tests drive the other direction
// with Register, Handshake, Send and Heartbeats.
package oscpsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cB-Sigma/evroaming/oscp"
)

// Received is one message the simulator accepted.
type Received struct {
	Endpoint    string
	Method      string
	RequestID   string
	Correlation string
	Header      http.Header
	Body        []byte
}

// Party is a simulated OSCP party.
type Party struct {
	Role string
	// HeartbeatInterval is asked of the other party in handshakes and
	// acknowledgements (seconds; 0 leaves it out).
	HeartbeatInterval int
	// RefuseHandshake answers handshakes and acknowledgements with 400.
	RefuseHandshake bool

	srv  *http.Server
	ln   net.Listener
	base string

	mu        sync.Mutex
	inTokens  map[string]bool // tokens accepted
	outToken  string          // token we send
	peerBase  string          // the other party's base URL
	received  []Received
	segments  []Received
	pending   map[string][]Received
	online    bool
	beatStop  chan struct{}
	lastError string
	faults    map[string]int
	delay     time.Duration
}

// Fail makes the party answer messages at endpoint ep ("" for every
// endpoint) with an HTTP status and an error body; 0 clears it.
func (p *Party) Fail(ep string, status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.faults == nil {
		p.faults = map[string]int{}
	}
	if status == 0 {
		delete(p.faults, ep)
	} else {
		p.faults[ep] = status
	}
}

// SetDelay makes the party wait d before answering.
func (p *Party) SetDelay(d time.Duration) { p.mu.Lock(); p.delay = d; p.mu.Unlock() }

// New starts a party of role on 127.0.0.1 accepting initialToken (the
// token it gave the other party out of band; "" for none).
func New(role, initialToken string) (*Party, error) {
	return NewAt("127.0.0.1:0", role, initialToken)
}

// NewAt is New on the address listen.
func NewAt(listen, role, initialToken string) (*Party, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	p := &Party{Role: role, ln: ln, inTokens: map[string]bool{}}
	if initialToken != "" {
		p.inTokens[initialToken] = true
	}
	p.base = fmt.Sprintf("http://%s/oscp/%s/%s", ln.Addr(), role, oscp.Version)
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// Close stops the party.
func (p *Party) Close() {
	p.StopHeartbeats()
	_ = p.srv.Close()
}

// BaseURL is the party's 2.0 base URL.
func (p *Party) BaseURL() string { return p.base }

// Online reports whether a handshake completed.
func (p *Party) Online() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.online
}

// Received returns the messages received at an endpoint ("" for all).
func (p *Party) Received(ep string) []Received {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Received
	for _, r := range p.received {
		if ep == "" || r.Endpoint == ep {
			out = append(out, r)
		}
	}
	return out
}

// Segments returns the segments received at an endpoint, in arrival order.
func (p *Party) Segments(ep string) []Received {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Received
	for _, r := range p.segments {
		if r.Endpoint == ep {
			out = append(out, r)
		}
	}
	return out
}

// Token is the token the party sends (the other party's latest).
func (p *Party) Token() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.outToken
}

func (p *Party) serve(w http.ResponseWriter, r *http.Request) {
	prefix := fmt.Sprintf("/oscp/%s/%s/", p.Role, oscp.Version)
	ep := oscp.CanonicalEndpoint(strings.TrimPrefix(r.URL.Path, prefix))
	tok := strings.TrimPrefix(r.Header.Get(oscp.HeaderAuthorization), "Token ")
	p.mu.Lock()
	ok := p.inTokens[tok]
	fault, delay := p.faults[ep], p.delay
	if fault == 0 {
		fault = p.faults[""]
	}
	p.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if fault != 0 {
		b, _ := json.Marshal(oscp.ErrorBody{Message: "simulated fault"})
		http.Error(w, string(b), fault)
		return
	}
	if !ok {
		http.Error(w, `{"message":"unknown token"}`, http.StatusUnauthorized)
		return
	}
	rid := r.Header.Get(oscp.HeaderRequestID)
	if rid == "" {
		http.Error(w, `{"message":"X-Request-ID is required"}`, http.StatusBadRequest)
		return
	}
	body, _ := io.ReadAll(r.Body)
	rec := Received{Endpoint: ep, Method: r.Method, RequestID: rid, Correlation: r.Header.Get(oscp.HeaderCorrelationID), Header: r.Header.Clone(), Body: body}
	if cnt := r.Header.Get(oscp.HeaderSegmentCount); cnt != "" {
		// A segment (4.1.2.1): kept until the last one arrives, then the
		// message is handled with the first segment's headers.
		key := rid
		if r.Header.Get(oscp.HeaderSegmentIndex) != "1" {
			key = rec.Correlation
		}
		p.mu.Lock()
		p.segments = append(p.segments, rec)
		if p.pending == nil {
			p.pending = map[string][]Received{}
		}
		p.pending[key] = append(p.pending[key], rec)
		parts := p.pending[key]
		p.mu.Unlock()
		if fmt.Sprint(len(parts)) != cnt {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		p.mu.Lock()
		delete(p.pending, key)
		p.mu.Unlock()
		var all []byte
		for _, s := range parts {
			all = append(all, s.Body...)
		}
		rec = parts[0]
		rec.Body, body, rid = all, all, parts[0].RequestID
	}
	if r.Method != http.MethodDelete {
		schema := map[string]string{
			oscp.EPRegister: oscp.SchemaRegister, oscp.EPHandshake: oscp.SchemaHandshake, oscp.EPHandshakeAcknowledge: oscp.SchemaHandshakeAcknowledge,
			oscp.EPHeartbeat: oscp.SchemaHeartbeat, oscp.EPUpdateGroupCapacityForecast: oscp.SchemaUpdateGroupCapacityForecast,
			oscp.EPAdjustGroupCapacityForecast: oscp.SchemaAdjustGroupCapacityForecast, oscp.EPGroupCapacityComplianceError: oscp.SchemaGroupCapacityComplianceError,
			oscp.EPUpdateGroupMeasurements: oscp.SchemaUpdateGroupMeasurements, oscp.EPUpdateAssetMeasurements: oscp.SchemaUpdateAssetMeasurements,
		}[ep]
		if schema == "" {
			http.Error(w, `{"message":"unknown endpoint"}`, http.StatusNotFound)
			return
		}
		if err := oscp.Validate(schema, body); err != nil {
			b, _ := json.Marshal(oscp.ErrorBody{Message: err.Error()})
			http.Error(w, string(b), http.StatusBadRequest)
			return
		}
	}
	var after func()
	switch ep {
	case oscp.EPRegister:
		var m oscp.Register
		_ = json.Unmarshal(body, &m)
		p.mu.Lock()
		first := p.outToken == ""
		if r.Method != http.MethodDelete {
			p.outToken, p.peerBase = m.Token, m.VersionURL[0].BaseURL
		}
		p.mu.Unlock()
		if first && r.Method == http.MethodPost {
			after = func() {
				tok := oscp.NewToken()
				p.mu.Lock()
				p.inTokens = map[string]bool{tok: true}
				p.mu.Unlock()
				_ = p.post(oscp.EPRegister, oscp.Register{Token: tok, VersionURL: []oscp.VersionURL{{Version: oscp.Version, BaseURL: p.base}}}, rid)
			}
		}
	case oscp.EPHandshake:
		if p.RefuseHandshake {
			http.Error(w, `{"message":"preferences refused"}`, http.StatusBadRequest)
			return
		}
		after = func() {
			if p.post(oscp.EPHandshakeAcknowledge, oscp.Handshake{RequiredBehaviour: p.behaviour()}, rid) == nil {
				p.mu.Lock()
				p.online = true
				p.mu.Unlock()
			}
		}
	case oscp.EPHandshakeAcknowledge:
		if p.RefuseHandshake {
			http.Error(w, `{"message":"preferences refused"}`, http.StatusBadRequest)
			return
		}
		p.mu.Lock()
		p.online = true
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.received = append(p.received, rec)
	p.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	if after != nil {
		w.(http.Flusher).Flush()
		go after()
	}
}

func (p *Party) behaviour() *oscp.RequiredBehaviour {
	rb := &oscp.RequiredBehaviour{MeasurementConfiguration: []string{}}
	if p.HeartbeatInterval > 0 {
		iv := p.HeartbeatInterval
		rb.HeartbeatInterval = &iv
	}
	return rb
}

// Register sends the party's Register first to a peer's base URL with
// the token the peer issued out of band.
func (p *Party) Register(peerBase, peerToken string) error {
	tok := oscp.NewToken()
	p.mu.Lock()
	p.inTokens[tok] = true
	p.peerBase, p.outToken = peerBase, peerToken
	p.mu.Unlock()
	return p.post(oscp.EPRegister, oscp.Register{Token: tok, VersionURL: []oscp.VersionURL{{Version: oscp.Version, BaseURL: p.base}}}, "")
}

// Handshake sends a Handshake.
func (p *Party) Handshake() error {
	return p.post(oscp.EPHandshake, oscp.Handshake{RequiredBehaviour: p.behaviour()}, "")
}

// Send posts a message to the peer's endpoint ep.
func (p *Party) Send(ep string, msg any, correlation string) error {
	return p.post(ep, msg, correlation)
}

// Heartbeats sends a heartbeat every interval with offline_mode_at ahead
// of now, until StopHeartbeats.
func (p *Party) Heartbeats(interval, ahead time.Duration) {
	p.StopHeartbeats()
	stop := make(chan struct{})
	p.mu.Lock()
	p.beatStop = stop
	p.mu.Unlock()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			at := time.Now().Add(ahead).UTC().Format("2006-01-02T15:04:05.000Z07:00")
			_ = p.post(oscp.EPHeartbeat, oscp.Heartbeat{OfflineModeAt: at}, "")
			select {
			case <-stop:
				return
			case <-t.C:
			}
		}
	}()
}

// StopHeartbeats stops sending heartbeats (a "server break down").
func (p *Party) StopHeartbeats() {
	p.mu.Lock()
	if p.beatStop != nil {
		close(p.beatStop)
		p.beatStop = nil
	}
	p.mu.Unlock()
}

// Post sends raw bytes with the given headers to the peer's endpoint and
// returns the status (for error-handling tests).
func (p *Party) Post(method, ep string, hdr http.Header, body []byte) (int, string) {
	p.mu.Lock()
	base := p.peerBase
	p.mu.Unlock()
	req, _ := http.NewRequest(method, base+"/"+ep, bytes.NewReader(body))
	req.Header = hdr
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (p *Party) post(ep string, msg any, correlation string) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	p.mu.Lock()
	base, tok := p.peerBase, p.outToken
	p.mu.Unlock()
	if base == "" {
		return errors.New("no peer base URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+ep, bytes.NewReader(body))
	req.Header.Set(oscp.HeaderAuthorization, "Token "+tok)
	req.Header.Set(oscp.HeaderRequestID, oscp.NewRequestID())
	req.Header.Set("Content-Type", "application/json")
	if correlation != "" {
		req.Header.Set(oscp.HeaderCorrelationID, correlation)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.setErr(err.Error())
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("%s: %d %s", ep, resp.StatusCode, b)
		p.setErr(err.Error())
		return err
	}
	return nil
}

func (p *Party) setErr(s string) {
	p.mu.Lock()
	p.lastError = s
	p.mu.Unlock()
}

// LastError is the latest failed send.
func (p *Party) LastError() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastError
}
