// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Package interop runs independent OCPI implementations against the
// simulators, in Docker. It is a separate module so that the simulators
// do not depend on it; its tests are skipped unless
// SIGMA_EVROAMING_INTEROP=1 is set and Docker answers.
package interop

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is the OCPI peer's image, built from ./peer.
const image = "sigma-evroaming-sim-interop:extrawest-ocpi-2025.7.16"

const envVar = "SIGMA_EVROAMING_INTEROP"

// needDocker skips the test unless the environment variable is 1 and
// Docker answers, and builds the peer image (from cache when unchanged).
func needDocker(t *testing.T) {
	t.Helper()
	if os.Getenv(envVar) != "1" {
		t.Skip("set " + envVar + "=1 to run the interop tests (needs Docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not running")
	}
	out, err := exec.Command("docker", "build", "-q", "-t", image, "peer").CombinedOutput()
	if err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// peer is the OCPI peer running in a container; its "PEER " lines are
// collected.
type peer struct {
	t    *testing.T
	name string
	port int

	mu  sync.Mutex
	log []string
}

// startPeer runs the peer with its HTTP port published on 127.0.0.1 and
// waits until it answers.
func startPeer(t *testing.T, tokenA string) *peer {
	t.Helper()
	p := &peer{t: t, name: fmt.Sprintf("sigma-evroaming-sim-%d", time.Now().UnixNano()), port: freePort(t)}
	cmd := exec.Command("docker", "run", "--rm", "--name", p.name,
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("127.0.0.1:%d:8000", p.port),
		"-e", fmt.Sprintf("OCPI_HOST=127.0.0.1:%d", p.port), "-e", "PROTOCOL=http",
		"-e", "COUNTRY_CODE=BE", "-e", "PARTY_ID=PER", "-e", "PEER_TOKEN_A="+tokenA,
		"-e", "COMMAND_AWAIT_TIME=1", "-e", "GET_ACTIVE_PROFILE_AWAIT_TIME=1", image)
	r, w := io.Pipe()
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			p.mu.Lock()
			p.log = append(p.log, sc.Text())
			p.mu.Unlock()
		}
	}()
	go func() { _ = cmd.Wait(); _ = w.Close() }()
	t.Cleanup(func() {
		_ = exec.Command("docker", "stop", "-t", "2", p.name).Run()
		if t.Failed() || os.Getenv(envVar+"_LOG") == "1" {
			t.Logf("peer output:\n%s", p.output())
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.url() + "/versions")
		if err == nil {
			resp.Body.Close()
			return p
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("the peer did not answer\n%s", p.output())
	return nil
}

func (p *peer) url() string { return fmt.Sprintf("http://127.0.0.1:%d/ocpi", p.port) }

func (p *peer) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.log, "\n")
}

// saw reports whether the peer printed a PEER line containing all parts.
func (p *peer) saw(parts ...string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
next:
	for _, l := range p.log {
		if !strings.HasPrefix(l, "PEER ") {
			continue
		}
		for _, part := range parts {
			if !strings.Contains(l, part) {
				continue next
			}
		}
		return true
	}
	return false
}

// httpsToHTTP is a test-only transport: the peer writes its pagination
// Link and CDR Location headers with https:// whatever it is served on,
// so requests to its own host are sent over plain HTTP.
type httpsToHTTP struct {
	host string
	rt   http.RoundTripper
}

func (h httpsToHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "https" && r.URL.Host == h.host {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme = "http"
		return h.rt.RoundTrip(r2)
	}
	return h.rt.RoundTrip(r)
}
