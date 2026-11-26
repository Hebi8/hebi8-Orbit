package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/Hebi8/hebi8-Orbit/internal/mcpserver"
)

func runMCPServe(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp-serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sockFlag  = fs.String("sock", "", "local runner Unix socket path (defaults to ORBIT_RUNNER_SOCK)")
		tokenFlag = fs.String("delegate-token", "", "per-session delegate token (defaults to ORBIT_DELEGATE_TOKEN)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	sock := strings.TrimSpace(*sockFlag)
	if sock == "" {
		sock = strings.TrimSpace(os.Getenv("ORBIT_RUNNER_SOCK"))
	}
	token := strings.TrimSpace(*tokenFlag)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("ORBIT_DELEGATE_TOKEN"))
	}

	backend := mcpserver.NewSocketBackend(sock, token)
	srv := mcpserver.New(backend, stderr)
	return srv.ServeStdio(ctx, stdin, stdout)
}
