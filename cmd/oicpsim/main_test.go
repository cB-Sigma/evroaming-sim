// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cB-Sigma/evroaming-sim/sim/oicpsim"
	"github.com/cB-Sigma/evroaming/oicp"
)

// The runner writes the PKI, and a CPO with that client certificate can
// push EVSE status through oicp.Client.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-operator", "DE*ABC", "-out", dir}, &out, func(s *oicpsim.Sim) {
			defer cancel()
			pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "cpo.pem"), filepath.Join(dir, "cpo-key.pem"))
			if err != nil {
				t.Error(err)
				return
			}
			ca, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(ca)
			c := oicp.Client{BaseURL: s.URL, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{pair}}}}}
			msg := oicp.PushEvseStatus{ActionType: oicp.ActionFullLoad, OperatorEvseStatus: oicp.OperatorEvseStatus{OperatorID: "DE*ABC",
				EvseStatusRecord: []oicp.EvseStatusRecord{{EvseID: "DE*ABC*E1", EvseStatus: oicp.EvseAvailable}}}}
			if _, err := c.PostAck(ctx, oicp.PathEvseStatus("DE*ABC"), msg); err != nil {
				t.Errorf("status push: %v", err)
			}
			if s.Status("DE*ABC*E1") != oicp.EvseAvailable {
				t.Errorf("status %q", s.Status("DE*ABC*E1"))
			}
		})
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
