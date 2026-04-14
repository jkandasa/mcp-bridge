// Command mcp-bridge is a meta-MCP server that:
//
//  1. Reads a YAML config listing child MCP servers (stdio binaries or
//     remote HTTP(S) network servers).
//  2. Manages each server: launches stdio binaries as subprocesses; connects
//     to network servers over HTTP(S).
//  3. Aggregates all discovered tools under namespaced names (<server>_<tool>).
//  4. Exposes the unified tool set over HTTP(S) using the MCP Streamable HTTP
//     transport, on the address and path configured in config.yaml.
//
// Usage:
//
//	mcp-bridge [-config /path/to/config.yaml]
//	mcp-bridge version
//
// Claude Code config:
//
//	{ "mcpServers": { "bridge": { "url": "http://localhost:7575/mcp" } } }
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"go.uber.org/zap"

	"mcp-bridge/internal/child"
	"mcp-bridge/internal/config"
	"mcp-bridge/internal/logger"
	"mcp-bridge/internal/mcp"
	"mcp-bridge/internal/network"
	"mcp-bridge/internal/router"
	"mcp-bridge/internal/tlsutil"
	"mcp-bridge/internal/version"
)

func main() {
	// Handle the "version" subcommand before flag parsing so that flags defined
	// for the server mode do not appear in version output or cause parse errors.
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.Get())
		return
	}

	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	v := version.Get()

	// Propagate the injected version into the MCP server identity so that
	// clients see the real release version during the initialize handshake.
	mcp.WrapperInfo.Version = v.Version

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Logger not ready yet — use a temporary zap to report the error.
		tmp, _ := zap.NewProduction()
		tmp.Fatal("failed to load config", zap.Error(err))
	}

	// Initialise zap from config before anything else logs.
	if err := logger.Init(cfg.Server.LogLevel); err != nil {
		tmp, _ := zap.NewProduction()
		tmp.Fatal("failed to init logger", zap.Error(err))
	}
	defer logger.Sync()

	log := logger.L()
	log.Info("mcp-bridge starting",
		zap.String("version", v.Version),
		zap.String("commit", v.GitCommit),
		zap.String("built", v.BuildDate),
		zap.String("go", v.GoVersion),
		zap.String("addr", cfg.Server.Addr),
		zap.String("path", cfg.Server.Path),
		zap.String("log_level", cfg.Server.LogLevel),
		zap.Bool("auth_enabled", cfg.Server.AuthToken != ""),
		zap.Bool("tls_enabled", cfg.Server.TLSEnabled()),
		zap.Int("servers", len(cfg.Servers)),
	)

	// ---------------------------------------------------------------------------
	// Resolve TLS configuration (nil = plain HTTP).
	// ---------------------------------------------------------------------------
	var tlsCfg *tls.Config
	switch {
	case cfg.Server.TLS.CertFile != "":
		tlsCfg, err = tlsutil.FromFiles(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		if err != nil {
			log.Fatal("failed to load TLS certificates", zap.Error(err))
		}
		log.Info("TLS enabled (custom certificate)",
			zap.String("cert", cfg.Server.TLS.CertFile),
			zap.String("key", cfg.Server.TLS.KeyFile),
		)
	case cfg.Server.TLS.AutoCert:
		tlsCfg, err = tlsutil.SelfSigned()
		if err != nil {
			log.Fatal("failed to generate self-signed certificate", zap.Error(err))
		}
		log.Info("TLS enabled (self-signed certificate)")
	}

	// ---------------------------------------------------------------------------
	// Build the tool router and wire up all servers.
	// ---------------------------------------------------------------------------
	rt := router.New()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// processes holds stdio child processes so we can Stop() them on shutdown.
	var processes []*child.Process

	// wg is used to wait for all startup goroutines to finish before logging
	// the "all servers started" line. Network servers are non-blocking (they
	// retry in the background) but we still wait for the initial attempt.
	var wg sync.WaitGroup

	for _, srv := range cfg.Servers {
		srv := srv // capture loop variable

		if srv.URL != "" {
			// ----------------------------------------------------------------
			// Network (HTTP/HTTPS) MCP server
			// ----------------------------------------------------------------
			cl := network.NewClient(
				srv.Name,
				srv.URL,
				srv.Headers,
				srv.RetryIntervalDuration(),
				srv.RequestTimeoutDuration(),
				srv.Insecure,
			)
			cl.ToolsRefreshed = func(serverName string, tools []mcp.Tool) {
				rt.Rebuild(serverName, tools, cl)
			}

			log.Info("connecting to network MCP server",
				zap.String("name", srv.Name),
				zap.String("url", srv.URL),
				zap.Bool("insecure", srv.Insecure),
			)

			wg.Add(1)
			go func() {
				defer wg.Done()
				// Initialize is non-blocking on failure — it starts a retry
				// loop internally and returns nil.
				if err := cl.Initialize(ctx); err != nil {
					log.Error("network server initialize error",
						zap.String("server", srv.Name),
						zap.Error(err),
					)
				}
			}()

		} else {
			// ----------------------------------------------------------------
			// Stdio MCP server (subprocess)
			// ----------------------------------------------------------------
			proc := child.NewProcess(srv.Name, srv.Command, srv.Args, srv.Env)
			processes = append(processes, proc)

			cl := child.NewClient(srv.Name, proc, ctx)
			cl.ToolsRefreshed = func(serverName string, tools []mcp.Tool) {
				rt.Rebuild(serverName, tools, cl)
			}

			log.Info("starting stdio MCP server",
				zap.String("name", srv.Name),
				zap.String("command", srv.Command),
			)

			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := proc.Start(ctx); err != nil {
					log.Error("stdio server failed to start",
						zap.String("server", srv.Name),
						zap.Error(err),
					)
					return
				}
				if err := cl.Initialize(ctx); err != nil {
					log.Error("stdio server failed to initialize",
						zap.String("server", srv.Name),
						zap.Error(err),
					)
					// Process stays alive; OnRestart will retry.
				}
			}()
		}
	}
	wg.Wait()

	log.Info("all servers started", zap.Int("tools", len(rt.Tools())))

	// ---------------------------------------------------------------------------
	// Run the bridge MCP HTTP server.
	// ---------------------------------------------------------------------------
	bridgeSrv := mcp.NewServer(rt, cfg.Server.Addr, cfg.Server.Path, cfg.Server.AuthToken, tlsCfg)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- bridgeSrv.Start(ctx)
	}()

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", zap.String("signal", sig.String()))
		cancel()
	case err := <-serverErr:
		if err != nil {
			log.Error("server exited with error", zap.Error(err))
		}
		cancel()
	}

	// Stop stdio child processes gracefully.
	if len(processes) > 0 {
		log.Info("stopping stdio child processes")
		for _, proc := range processes {
			proc.Stop()
		}
	}
	log.Info("shutdown complete")
}
