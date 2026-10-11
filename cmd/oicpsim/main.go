// Copyright 2026 Abhinav Gautam
// SPDX-License-Identifier: Apache-2.0

// Command oicpsim serves a simulated Hubject-like OICP 2.3 platform for a
// charge point operator's OICP interface, over mutual TLS with a
// throw-away certificate authority. For example:
//
//	oicpsim -listen 127.0.0.1:9443 -operator 'DE*ABC' -out ./oicpsim-pki
//
// It writes the authority's certificate (ca.pem) and a client certificate
// and key for the operator (cpo.pem, cpo-key.pem) to -out, prints its base
// URL and serves until interrupted. Cards are demo values: -card UID
// authorises one RFID card for the EMP DE-XYZ (repeatable).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cB-Sigma/evroaming-sim/sim/oicpsim"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, nil); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "oicpsim:", err)
		os.Exit(1)
	}
}

type cards []string

func (c *cards) String() string     { return strings.Join(*c, ",") }
func (c *cards) Set(v string) error { *c = append(*c, v); return nil }

// run parses args, starts the simulator, calls ready (when set) with it
// and serves until ctx ends.
func run(ctx context.Context, args []string, out io.Writer, ready func(*oicpsim.Sim)) error {
	fs := flag.NewFlagSet("oicpsim", flag.ContinueOnError)
	fs.SetOutput(out)
	listen := fs.String("listen", "127.0.0.1:0", "address to serve on")
	operator := fs.String("operator", "DE*ABC", "the CPO's OperatorID, the common name of its client certificate")
	dir := fs.String("out", "oicpsim-pki", "directory for ca.pem, cpo.pem and cpo-key.pem")
	var cs cards
	fs.Var(&cs, "card", "an RFID card UID Hubject authorises (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pki, err := oicpsim.NewPKI()
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := pki.Issue(*operator)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	for name, v := range map[string]string{"ca.pem": pki.CAPEM, "cpo.pem": certPEM, "cpo-key.pem": keyPEM} {
		if err := os.WriteFile(filepath.Join(*dir, name), []byte(v), 0o600); err != nil {
			return err
		}
	}
	cfg := oicpsim.Config{PKI: pki, Cards: map[string]oicpsim.Card{}, Listen: *listen}
	for _, c := range cs {
		cfg.Cards[c] = oicpsim.Card{ProviderID: "DE-XYZ"}
	}
	s, err := oicpsim.Start(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(out, "OICP 2.3 at %s (mutual TLS); CA and the client certificate of %s in %s\n", s.URL, *operator, *dir)
	if ready != nil {
		ready(s)
	}
	<-ctx.Done()
	return nil
}
