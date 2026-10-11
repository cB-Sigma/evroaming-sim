// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cB-Sigma/evroaming-sim/sim/ocpisim"
	"github.com/cB-Sigma/evroaming/ocpi"
)

// The runner starts the simulator; its versions endpoint answers the
// registration token and refuses another.
func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-token-a", "tok-a"}, &out, func(s *ocpisim.Sim) {
			defer cancel()
			var c ocpi.Client
			rep, err := c.Send(ctx, ocpi.Request{Method: http.MethodGet, URL: s.VersionsURL()}, "tok-a", false)
			if err != nil {
				t.Errorf("versions: %v", err)
				return
			}
			var vs []ocpi.VersionInfo
			if err := json.Unmarshal(rep.Envelope.Data, &vs); err != nil || len(vs) == 0 || vs[0].Version != ocpi.Version {
				t.Errorf("versions %s: %v", rep.Envelope.Data, err)
			}
			if _, err := c.Send(ctx, ocpi.Request{Method: http.MethodGet, URL: s.VersionsURL()}, "other", false); err == nil {
				t.Error("an unknown token was accepted")
			}
		})
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "versions at http://127.0.0.1:") {
		t.Errorf("output %q", out.String())
	}
}
