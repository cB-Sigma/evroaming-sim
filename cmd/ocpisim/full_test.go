// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/cB-Sigma/evroaming-sim/sim/ocpisim"
	"github.com/cB-Sigma/evroaming/ocpi"
)

// -full serves a party with every module, here a CPO.
func TestRunFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { partyReady = nil }()
	var eps []ocpi.Endpoint
	partyReady = func(p *ocpisim.Party) {
		defer cancel()
		c := &ocpi.Client{}
		do := func(ctx context.Context, r ocpi.Request) (*ocpi.Reply, error) { return c.Send(ctx, r, r.Token, false) }
		_, e, err := ocpi.FetchEndpoints(ctx, do, p.VersionsURL(), "tok-a", nil)
		if err != nil {
			t.Error(err)
		}
		eps = e
	}
	if err := run(ctx, []string{"-full", "-role", "CPO", "-token-a", "tok-a"}, &bytes.Buffer{}, nil); err != nil {
		t.Fatal(err)
	}
	if len(eps) != 17 {
		t.Errorf("%d endpoints", len(eps))
	}
}
