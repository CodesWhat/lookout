package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/codeswhat/portwing/internal/adapter"
	"github.com/codeswhat/portwing/internal/adapter/drydock"
	"github.com/codeswhat/portwing/internal/audit"
	"github.com/codeswhat/portwing/internal/auth"
	"github.com/codeswhat/portwing/internal/banner"
	"github.com/codeswhat/portwing/internal/config"
	"github.com/codeswhat/portwing/internal/docker"
	"github.com/codeswhat/portwing/internal/edge"
	"github.com/codeswhat/portwing/internal/generic"
	applog "github.com/codeswhat/portwing/internal/log"
	"github.com/codeswhat/portwing/internal/protocol"
	"github.com/codeswhat/portwing/internal/server"
)

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) >= 2 && args[1] == "hash-token" {
		return runHashToken(stdin, stdout, stderr)
	}

	if len(args) >= 2 && args[1] == "keygen" {
		return runKeygen(args[2:], stdout, stderr)
	}

	if len(args) >= 2 && (args[1] == "version" || args[1] == "--version" || args[1] == "-V") {
		fmt.Fprintln(stdout, protocol.AgentVersion)
		return 0
	}

	if len(args) >= 2 && (args[1] == "--help" || args[1] == "-h") {
		printUsage(stdout)
		return 0
	}

	if len(args) >= 2 && strings.HasPrefix(args[1], "-") {
		fmt.Fprintf(stderr, "portwing: unrecognized flag %q\n\n", args[1])
		printUsage(stderr)
		return 1
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}

	banner.Render(stderr, banner.Info{
		Version: protocol.AgentVersion,
		Mode:    modeString(cfg),
		Adapter: cfg.Adapter,
	})

	applog.SetupLogger(cfg.LogLevel)

	slog.Info("starting portwing", "version", protocol.AgentVersion, "mode", modeString(cfg))

	dockerClient, err := docker.NewClient(cfg.DockerSocket, cfg.RequestTimeout)
	if err != nil {
		slog.Error("failed to create docker client", "error", err)
		return 1
	}

	version, err := dockerClient.GetVersion(context.Background())
	if err != nil {
		slog.Error("failed to connect to docker", "error", err)
		return 1
	}
	slog.Info("connected to docker", "version", version)

	// Select adapter.
	a := selectAdapter(cfg, dockerClient)
	slog.Info("adapter selected", "adapter", a.Name())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	if cfg.IsEdgeMode() {
		slog.Info("starting in edge mode", "url", cfg.DrydockURL)
		auditor, auditClose, err := audit.New(cfg.AuditLog, cfg.AuditBufferSize)
		if err != nil {
			slog.Error("failed to open audit log", "error", err)
			return 1
		}
		defer auditClose()
		edgeClient := edge.NewClient(cfg, dockerClient, a, auditor)
		go func() {
			<-sigCh
			slog.Info("shutting down...")
			cancel()
		}()
		if err := edgeClient.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("edge client error", "error", applog.Sanitize(err.Error()))
			return 1
		}
	} else {
		slog.Info("starting in standard mode", "address", config.ListenAddress(cfg.BindAddress, cfg.Port))
		srv, err := server.NewServer(cfg, dockerClient, a)
		if err != nil {
			slog.Error("failed to create server", "error", err)
			return 1
		}
		// shutdownDone is closed once the signal-triggered Shutdown call
		// returns (drained or timed out). ListenAndServe unblocks the
		// instant Shutdown closes the listener, well before in-flight
		// handlers finish, so run() must join this channel rather than
		// returning as soon as ListenAndServe does — otherwise main()'s
		// os.Exit races ahead of active handlers (e.g. a compose deploy)
		// and cuts them off mid-request.
		shutdownDone := make(chan struct{})
		go func() {
			defer close(shutdownDone)
			<-sigCh
			slog.Info("shutting down...")
			cancel()
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutdownCancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				slog.Warn("server shutdown error", "error", err)
			}
		}()
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			return 1
		}
		// A nil or ErrServerClosed result only happens once Shutdown has
		// closed the listener, i.e. after the signal fired, so this always
		// completes; it just waits for the drain (or its timeout) too.
		<-shutdownDone
	}

	slog.Info("portwing stopped")
	return 0
}

// printUsage writes top-level CLI usage to w. Portwing is configured
// entirely through environment variables rather than flags; see
// docs/content/docs/configuration.mdx for the full reference.
func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: portwing [command]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "With no command, portwing starts the agent, configured entirely through")
	fmt.Fprintln(w, "environment variables (PORT, TOKEN, DRYDOCK_URL, ...). See")
	fmt.Fprintln(w, "https://portwing.codeswhat.com/docs/configuration for the full reference.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  hash-token   hash a token from stdin for use as TOKEN_HASH")
	fmt.Fprintln(w, "  keygen       generate an Ed25519 keypair for AUTHORIZED_KEYS auth")
	fmt.Fprintln(w, "  version      print the agent version")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  -h, --help      show this help message")
	fmt.Fprintln(w, "  -V, --version   print the agent version")
}

func selectAdapter(cfg *config.Config, dockerClient *docker.Client) adapter.Adapter {
	info := drydock.AgentInfo{
		LogLevel:     cfg.LogLevel,
		PollInterval: (time.Duration(cfg.DDPollInterval) * time.Second).String(),
	}
	switch cfg.Adapter {
	case "generic":
		return generic.New(dockerClient, cfg.AgentName)
	case "drydock":
		return drydock.NewAdapter(dockerClient, cfg.AgentName, info)
	default:
		slog.Warn("unknown adapter, falling back to drydock", "adapter", cfg.Adapter)
		return drydock.NewAdapter(dockerClient, cfg.AgentName, info)
	}
}

func modeString(cfg *config.Config) string {
	if cfg.IsEdgeMode() {
		return "edge"
	}
	return "standard"
}

// runKeygen generates an Ed25519 keypair and prints the private key in PKCS#8
// PEM format and the authorized_keys line to stdout. Prompts are written to
// stderr so stdout output is unambiguous and pipe-friendly.
//
// Usage:
//
//	portwing keygen [-comment <text>]
//	portwing keygen -pub-from <private.pem> [-comment <text>]
func runKeygen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	comment := fs.String("comment", "", "Comment to embed in the authorized_keys line (optional)")
	pubFrom := fs.String("pub-from", "", "Re-derive the authorized_keys line from an existing private key PEM file")

	// Print usage to stderr.
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: portwing keygen [-comment <text>]")
		fmt.Fprintln(stderr, "       portwing keygen -pub-from <private.pem> [-comment <text>]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Generates an Ed25519 keypair for use with AUTHORIZED_KEYS authentication.")
		fmt.Fprintln(stderr, "The private key (PEM PKCS#8) and authorized_keys line are written to stdout.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *pubFrom != "" {
		// Re-derive the authorized_keys line from an existing private key.
		priv, err := auth.LoadPrivateKey(*pubFrom)
		if err != nil {
			fmt.Fprintf(stderr, "keygen: %v\n", err)
			return 1
		}
		pub := priv.Public().(ed25519.PublicKey)
		line := auth.AuthorizedKeyLine(pub, *comment)
		fmt.Fprintln(stderr, "# authorized_keys line (add to AUTHORIZED_KEYS file on agent host):")
		fmt.Fprintln(stdout, line)
		return 0
	}

	// Generate a new keypair.
	fmt.Fprintln(stderr, "Generating Ed25519 keypair...")
	privPEM, authKeyLine, err := auth.GenerateKeyPair(*comment)
	if err != nil {
		fmt.Fprintf(stderr, "keygen: %v\n", err)
		return 1
	}

	fmt.Fprintln(stderr, "# Private key (PKCS#8 PEM) — store securely; set as PRIVATE_KEY_FILE on the client:")
	fmt.Fprint(stdout, string(privPEM))
	fmt.Fprintln(stderr, "# authorized_keys line — add to AUTHORIZED_KEYS file on agent host:")
	fmt.Fprintln(stdout, authKeyLine)
	return 0
}

// runHashToken reads a token from stdin, hashes it with Argon2id, and prints
// the resulting PHC string. The result can be stored as TOKEN_HASH.
func runHashToken(stdin io.Reader, stdout, stderr io.Writer) int {
	fmt.Fprint(stderr, "Enter token: ")
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(stderr, "error reading input: %v\n", err)
		} else {
			fmt.Fprintln(stderr, "no input provided")
		}
		return 1
	}
	token := strings.TrimSpace(scanner.Text())
	if token == "" {
		fmt.Fprintln(stderr, "token must not be empty")
		return 1
	}
	phc, err := server.HashToken(token)
	if err != nil {
		fmt.Fprintf(stderr, "hash-token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, phc)
	return 0
}
