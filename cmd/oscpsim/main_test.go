// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/cB-Sigma/evroaming-sim/sim/oscpsim"
	"github.com/cB-Sigma/evroaming/oscp"
)

// Two runners register with each other: an fp started with -peer
// registers with a cp, which records the Register.
func TestRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cpReady := make(chan *oscpsim.Party, 1)
	go func() {
		_ = run(ctx, []string{"-role", "cp", "-token", "tok-for-fp"}, &bytes.Buffer{}, func(p *oscpsim.Party) { cpReady <- p })
	}()
	cp := <-cpReady
	fpCtx, fpCancel := context.WithCancel(ctx)
	var out bytes.Buffer
	err := run(fpCtx, []string{"-role", "fp", "-peer", cp.BaseURL(), "-peer-token", "tok-for-fp"}, &out, func(*oscpsim.Party) { fpCancel() })
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := cp.Received(oscp.EPRegister); len(got) != 1 {
		t.Errorf("the cp received %d Register messages", len(got))
	}
	if err := run(ctx, []string{"-role", "xx"}, &bytes.Buffer{}, nil); err == nil {
		t.Error("an unknown role was accepted")
	}
}
