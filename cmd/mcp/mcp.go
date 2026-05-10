package mcp

import (
	"net/http"

	"github.com/paularlott/logger"
	mcplib "github.com/paularlott/mcp"
)

var toolRegistrations []func(*mcplib.Server)

func RegisterTool(fn func(*mcplib.Server)) {
	toolRegistrations = append(toolRegistrations, fn)
}

func StartMCPServer(log logger.Logger) {
	server := mcplib.NewServer("skopos-mcp", "1.0.0")

	for _, fn := range toolRegistrations {
		fn(server)
	}

	go func() {
		log.Info("starting MCP server on :9000")
		http.HandleFunc("/mcp", server.HandleRequest)
		if err := http.ListenAndServe(":9000", nil); err != nil {
			log.Error("MCP server error", "error", err)
		}
	}()
}
