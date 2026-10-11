// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Command oscpsim serves a simulated OSCP 2.0 party (a capacity provider
// cp, flexibility provider fp or capacity optimizer co), so another
// party's OSCP interface can be tried without a real one. For example:
//
//	oscpsim -role cp -listen 127.0.0.1:9090 -token demo-token-for-the-fp
//	oscpsim -role fp -peer http://127.0.0.1:8080/oscp/cp/2.0 -peer-token <token the cp gave> -heartbeat 30s
//
// It prints its base URL and the token it accepts until registration
// replaces it, registers with -peer when given (then shakes hands when
// -handshake is set, and sends heartbeats every -heartbeat), and
// serves until interrupted. Tokens are demo values.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/cB-Sigma/evroaming-sim/sim/oscpsim"
	"github.com/cB-Sigma/evroaming/oscp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "oscpsim:", err)
		os.Exit(1)
	}
}

// run parses args, starts the party, calls ready (when set) with it and
// serves until ctx ends.
func run(ctx context.Context, args []string, out io.Writer, ready func(*oscpsim.Party)) error {
	fs := flag.NewFlagSet("oscpsim", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "127.0.0.1:0", "address to serve on")
	role := fs.String("role", oscp.RoleCP, "cp, fp or co")
	token := fs.String("token", "demo-initial-token", "the token another party registers with")
	peer := fs.String("peer", "", "another party's base URL to register with")
	peerToken := fs.String("peer-token", "", "the token that party gave us (with -peer)")
	shake := fs.Bool("handshake", false, "send a Handshake after registering (a cp to its fp, an fp to its co)")
	hb := fs.Duration("heartbeat", 0, "send heartbeats this often after registering (0: none)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *role {
	case oscp.RoleCP, oscp.RoleFP, oscp.RoleCO:
	default:
		return fmt.Errorf("-role is cp, fp or co, not %q", *role)
	}
	p, err := oscpsim.NewAt(*listen, *role, *token)
	if err != nil {
		return err
	}
	defer p.Close()
	fmt.Fprintf(out, "OSCP 2.0 %s at %s, accepting token %q\n", *role, p.BaseURL(), *token)
	if *peer != "" {
		if err := p.Register(*peer, *peerToken); err != nil {
			return fmt.Errorf("registering with %s: %w", *peer, err)
		}
		// Handshakes go from a cp to its fp and from an fp to its co
		// (2.6.2); -handshake says this party starts one.
		if *shake {
			if err := p.Handshake(); err != nil {
				fmt.Fprintln(out, "handshake:", err)
			}
		}
		if *hb > 0 {
			p.Heartbeats(*hb, 3**hb)
			defer p.StopHeartbeats()
		}
		fmt.Fprintln(out, "registered with", *peer)
	}
	if ready != nil {
		ready(p)
	}
	<-ctx.Done()
	return nil
}
