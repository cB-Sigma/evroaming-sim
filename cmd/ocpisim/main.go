// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Command ocpisim serves a simulated OCPI 2.2.1 partner platform (an eMSP,
// or a hub with -role HUB), so a charge point operator's OCPI interface
// can be tried without a roaming partner. For example:
//
//	ocpisim -listen 127.0.0.1:9080 -country NL -party EMS -token-a demo-token-a
//	ocpisim -register http://127.0.0.1:8080/ocpi/versions -cpo-token <token A of the CPO>
//
// It prints its versions URL and the registration token (token A) a CPO
// uses to register with it, or registers with the CPO itself when
// -register is given, and serves until interrupted. Tokens are demo
// values.
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

	"github.com/cB-Sigma/evroaming-sim/sim/ocpisim"
	"github.com/cB-Sigma/evroaming/ocpi"
)

// partyReady, when set (by tests), is called with the party of -full.
var partyReady func(*ocpisim.Party)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "ocpisim:", err)
		os.Exit(1)
	}
}

// run parses args, starts the simulator, calls ready (when set) with it
// and serves until ctx ends.
func run(ctx context.Context, args []string, out io.Writer, ready func(*ocpisim.Sim)) error {
	fs := flag.NewFlagSet("ocpisim", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "127.0.0.1:0", "address to serve on")
	role := fs.String("role", "EMSP", "EMSP or HUB")
	country := fs.String("country", "NL", "our country code")
	party := fs.String("party", "EMS", "our party ID")
	tokenA := fs.String("token-a", "demo-token-a", "the registration token a CPO uses with us")
	raw := fs.Bool("raw", false, "send and expect tokens without Base64 (as OCPI 2.1.1 did)")
	page := fs.Int("page", 2, "most objects per page we serve")
	register := fs.String("register", "", "a CPO's versions URL to register with")
	cpoToken := fs.String("cpo-token", "", "the registration token the CPO gave us (with -register)")
	full := fs.Bool("full", false, "serve every module in both interface roles (ocpisim.Party; -role may also be CPO, HUB or SCSP)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *full {
		p, err := ocpisim.NewParty(ocpisim.PartyConfig{Self: ocpi.Party{CountryCode: *country, PartyID: *party}, Role: *role,
			TokenA: *tokenA, PageLimit: *page, Listen: *listen})
		if err != nil {
			return err
		}
		defer p.Close()
		fmt.Fprintf(out, "OCPI 2.2.1 %s %s/%s (every module): versions at %s, registration token %q\n", *role, *country, *party, p.VersionsURL(), *tokenA)
		if *register != "" {
			if err := p.Register(ctx, *register, *cpoToken); err != nil {
				return fmt.Errorf("registering with %s: %w", *register, err)
			}
			fmt.Fprintln(out, "registered with", *register)
		}
		if partyReady != nil {
			partyReady(p)
		}
		<-ctx.Done()
		return nil
	}
	s, err := ocpisim.Start(ocpisim.Config{Role: *role, CountryCode: *country, PartyID: *party, BusinessName: "ocpisim",
		TokenA: *tokenA, RawTokens: *raw, PageSize: *page, Listen: *listen})
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(out, "OCPI 2.2.1 %s %s/%s: versions at %s, registration token %q\n", *role, *country, *party, s.VersionsURL(), *tokenA)
	if *register != "" {
		if err := s.Register(ctx, *register, *cpoToken); err != nil {
			return fmt.Errorf("registering with %s: %w", *register, err)
		}
		fmt.Fprintln(out, "registered with", *register)
	}
	if ready != nil {
		ready(s)
	}
	<-ctx.Done()
	return nil
}
