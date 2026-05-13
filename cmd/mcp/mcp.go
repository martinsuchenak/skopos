package mcp

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/paularlott/logger"
	mcplib "github.com/paularlott/mcp"
)

var toolRegistrations []func(*mcplib.Server, *status.Service)
var blackboardToolRegistrations []func(*mcplib.Server, *blackboard.Service)

func RegisterTool(fn func(*mcplib.Server, *status.Service)) {
	toolRegistrations = append(toolRegistrations, fn)
}

func RegisterBlackboardTool(fn func(*mcplib.Server, *blackboard.Service)) {
	blackboardToolRegistrations = append(blackboardToolRegistrations, fn)
}

func StartMCPServer(log logger.Logger, statusService *status.Service, blackboardService *blackboard.Service) {
	server := mcplib.NewServer("skopos-mcp", "1.0.0")

	for _, fn := range toolRegistrations {
		fn(server, statusService)
	}

	for _, fn := range blackboardToolRegistrations {
		fn(server, blackboardService)
	}

	go func() {
		log.Info("starting MCP server on :9000")
		http.HandleFunc("/mcp", server.HandleRequest)
		if err := http.ListenAndServe(":9000", nil); err != nil {
			log.Error("MCP server error", "error", err)
		}
	}()
}
