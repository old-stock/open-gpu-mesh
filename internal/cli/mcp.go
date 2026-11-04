package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/old-stock/open-gpu-mesh/internal/mcp"
)

// cmdMCP runs an MCP server on stdin/stdout using the saved login. stdout
// carries only JSON-RPC; diagnostics go to stderr.
func cmdMCP(e *env, args []string) {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stderr, "usage: ogm mcp [--project NAME]\n\nRuns a Model Context Protocol server on stdio for Claude Code/Desktop, Cursor and other MCP clients.\nIt uses your saved login (ogm login / ogm join) or OGM_SERVER, OGM_API_KEY, OGM_PROJECT.\n\n  claude mcp add ogm -- ogm mcp")
		return
	}
	if e.cfg.Server == "" || e.cfg.APIKey == "" {
		fatalf("not logged in: run `ogm login <server> --key <api-key>` first (or set OGM_SERVER and OGM_API_KEY)")
	}
	srv := mcp.New(mcp.NewHTTPCaller(e.cfg.Server, e.cfg.APIKey), e.project)
	level := slog.LevelWarn
	if os.Getenv("OGM_MCP_DEBUG") != "" {
		level = slog.LevelDebug
	}
	srv.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	srv.Logger.Info("ogm mcp: serving on stdio", "server", e.cfg.Server, "project", e.project)
	if err := srv.ServeStdio(e.ctx, os.Stdin, os.Stdout); err != nil && e.ctx.Err() == nil {
		fatalf("mcp: %v", err)
	}
}
