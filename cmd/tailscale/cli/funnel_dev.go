// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

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
		ShortHelp: "Run Tailscale Funnel on your device",
		ShortUsage: strings.Join([]string{
			"funnel <port> [--background]",
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
		FlagSet: e.newFlags("funnel", func(fs *flag.FlagSet) {
			fs.BoolVar(&e.background, "background", false, "run the command in the background")
		}),
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
func (e *serveEnv) runFunnelDev(ctx context.Context, args []string) (err error) {
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
	req := ipn.ServeStreamRequest{
		HostPort:   hp,
		Source:     source,
		MountPoint: "/", // TODO(marwan-at-work): support multiple mount points
	}
	revertServe, err := e.setServe(ctx, req)
	if err != nil {
		return fmt.Errorf("error setting serve: %w", err)
	}
	if e.background {
		return nil
	}
	defer func() { errors.Join(err, revertServe()) }()

	// In the streaming case, the process stays running in the
	// foreground and prints out connections to the HostPort.
	//
	// The local backend handles updating the ServeConfig as
	// necessary, then restores it to its original state once
	// the process's context is closed or the client turns off
	// Tailscale.
	return e.streamServe(ctx, req)
}

func (e *serveEnv) streamServe(ctx context.Context, req ipn.ServeStreamRequest) (err error) {
	stream, err := e.lc.StreamServe(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	fmt.Fprintf(os.Stderr, "Funnel started on \"https://%s\".\n", strings.TrimSuffix(string(req.HostPort), ":443"))
	fmt.Fprintf(os.Stderr, "Press Ctrl-C to stop Funnel.\n\n")
	_, err = io.Copy(os.Stdout, stream)
	return err
}

func (e *serveEnv) setServe(ctx context.Context, req ipn.ServeStreamRequest) (func() error, error) {
	sc, err := e.lc.GetServeConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting serve config: %w", err)
	}
	setHandler(sc, req, !e.background)
	if err := e.lc.SetServeConfig(ctx, sc); err != nil {
		return nil, fmt.Errorf("errro setting serve config: %w", err)
	}
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sc, err := e.lc.GetServeConfig(ctx)
		if err != nil {
			return fmt.Errorf("error getting config: %w", err)
		}
		deleteHandler(sc, req, 443)
		return e.lc.SetServeConfig(ctx, sc)
	}, nil
}

func setHandler(sc *ipn.ServeConfig, req ipn.ServeStreamRequest, ephemeral bool) {
	if sc.TCP == nil {
		sc.TCP = make(map[uint16]*ipn.TCPPortHandler)
	}
	if _, ok := sc.TCP[443]; !ok {
		sc.TCP[443] = &ipn.TCPPortHandler{
			HTTPS:     true,
			Ephemeral: ephemeral,
		}
	}
	if sc.Web == nil {
		sc.Web = make(map[ipn.HostPort]*ipn.WebServerConfig)
	}
	wsc, ok := sc.Web[req.HostPort]
	if !ok {
		wsc = &ipn.WebServerConfig{}
		sc.Web[req.HostPort] = wsc
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

func deleteHandler(sc *ipn.ServeConfig, req ipn.ServeStreamRequest, port uint16) {
	delete(sc.AllowFunnel, req.HostPort)
	if sc.TCP != nil {
		delete(sc.TCP, port)
	}
	if sc.Web == nil {
		return
	}
	if sc.Web[req.HostPort] == nil {
		return
	}
	wsc, ok := sc.Web[req.HostPort]
	if !ok {
		return
	}
	if wsc.Handlers == nil {
		return
	}
	if _, ok := wsc.Handlers[req.MountPoint]; !ok {
		return
	}
	delete(wsc.Handlers, req.MountPoint)
	if len(wsc.Handlers) == 0 {
		delete(sc.Web, req.HostPort)
	}
}
