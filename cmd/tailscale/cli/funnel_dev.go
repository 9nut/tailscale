// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/ipn"
)

// newFunnelDevCommand returns a new "funnel" subcommand using e as its environment.
// The funnel subcommand is used to turn on/off the Funnel service.
// Funnel is off by default.
// Funnel allows you to publish a 'tailscale serve' server publicly,
// open to the entire internet.
// newFunnelCommand shares the same serveEnv as the "serve" subcommand.
// See newServeCommand and serve.go for more details.
func newFunnelDevCommand(e *serveEnv) *ffcli.Command {
	return &ffcli.Command{
		Name:      "funnel",
		ShortHelp: "Turn on/off Funnel service",
		ShortUsage: strings.Join([]string{
			"funnel <port>",
			"funnel status [--json]",
		}, "\n  "),
		LongHelp: strings.Join([]string{
			"Funnel allows you to expose your local",
			"server publicly to the entire internet.",
			"Note that it only supports https servers at this point.",
			"This command is in development and is unsupported",
		}, "\n"),
		Exec:      e.runFunnelDev,
		UsageFunc: usageFunc,
		Subcommands: []*ffcli.Command{
			{
				Name:      "status",
				Exec:      e.runServeStatus,
				ShortHelp: "show current serve/Funnel status",
				FlagSet: e.newFlags("funnel-status", func(fs *flag.FlagSet) {
					fs.BoolVar(&e.json, "json", false, "output JSON")
				}),
				UsageFunc: usageFunc,
			},
		},
	}
}

// runFunnelDev is the entry point for the "tailscale funnel" subcommand and
// manages turning on/off Funnel. Funnel is off by default.
//
// Note: funnel is only supported on single DNS name for now. (2023-08-18)
func (e *serveEnv) runFunnelDev(ctx context.Context, args []string) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()
	if len(args) != 1 {
		return flag.ErrHelp
	}
	var source string
	port64, err := strconv.ParseUint(args[0], 10, 16)
	if err == nil {
		source = fmt.Sprintf("http://127.0.0.1:%d", port64)
	} else {
		source, err = expandProxyTarget(args[0])
	}
	if err != nil {
		return err
	}

	st, err := e.getLocalClientStatusWithoutPeers(ctx)
	if err != nil {
		return fmt.Errorf("getting client status: %w", err)
	}

	if err := e.verifyFunnelEnabled(ctx, st, 443); err != nil {
		return err
	}

	dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
	hp := ipn.HostPort(dnsName + ":443") // TODO(marwan-at-work): support the 2 other ports

	// In the streaming case, the process stays running in the
	// foreground and prints out connections to the HostPort.
	//
	// The local backend handles updating the ServeConfig as
	// necessary, then restores it to its original state once
	// the process's context is closed or the client turns off
	// Tailscale.
	return e.streamServe(ctx, ipn.ServeStreamRequest{
		HostPort:   hp,
		Source:     source,
		MountPoint: "/", // TODO(marwan-at-work): support multiple mount points
	})
}

func (e *serveEnv) streamServe(ctx context.Context, req ipn.ServeStreamRequest) error {
	watcher, err := e.lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return err
	}
	defer watcher.Close()
	n, err := watcher.Next()
	if err != nil {
		return err
	}
	if n.SessionID == "" {
		return errors.New("missing session id")
	}
	sc, err := e.lc.GetServeConfig(ctx)
	if err != nil {
		return fmt.Errorf("error getting serve config: %w", err)
	}
	setHandler(sc, req, n.SessionID)
	err = e.lc.SetServeConfig(ctx, sc)
	if err != nil {
		return fmt.Errorf("error setting serve config: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Funnel started on \"https://%s\".\n", strings.TrimSuffix(string(req.HostPort), ":443"))
	fmt.Fprintf(os.Stderr, "Press Ctrl-C to stop Funnel.\n\n")

	for {
		n, err := watcher.Next()
		if err != nil {
			return fmt.Errorf("error calling next: %w", err)
		}
		if n.FunnelRequestLog == nil {
			continue
		}
		bts, _ := json.Marshal(n.FunnelRequestLog)
		fmt.Printf("%s\n", bts)
	}
}

func setHandler(sc *ipn.ServeConfig, req ipn.ServeStreamRequest, sessionID string) {
	if sc.Foreground == nil {
		sc.Foreground = make(map[string]*ipn.ServeConfig)
	}
	if sc.Foreground[sessionID] == nil {
		sc.Foreground[sessionID] = &ipn.ServeConfig{}
	}
	if sc.Foreground[sessionID].TCP == nil {
		sc.Foreground[sessionID].TCP = make(map[uint16]*ipn.TCPPortHandler)
	}
	if _, ok := sc.Foreground[sessionID].TCP[443]; !ok {
		sc.Foreground[sessionID].TCP[443] = &ipn.TCPPortHandler{HTTPS: true}
	}
	if sc.Foreground[sessionID].Web == nil {
		sc.Foreground[sessionID].Web = make(map[ipn.HostPort]*ipn.WebServerConfig)
	}
	wsc, ok := sc.Foreground[sessionID].Web[req.HostPort]
	if !ok {
		wsc = &ipn.WebServerConfig{}
		sc.Foreground[sessionID].Web[req.HostPort] = wsc
	}
	if wsc.Handlers == nil {
		wsc.Handlers = make(map[string]*ipn.HTTPHandler)
	}
	wsc.Handlers[req.MountPoint] = &ipn.HTTPHandler{
		Proxy: req.Source,
	}
	if sc.AllowFunnel == nil {
		sc.AllowFunnel = make(map[ipn.HostPort]bool)
	}
	sc.AllowFunnel[req.HostPort] = true
}
