package mcp

import (
	"context"
	"fmt"

	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterTool(registerSampleTool)
}

func registerSampleTool(server *mcplib.Server) {
	server.RegisterTool(
		mcplib.NewTool("sample-tool", "A sample MCP tool for skopos",
			mcplib.String("input", "Sample input parameter"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := req.StringOr("input", "world")
			return mcplib.NewToolResponseText(fmt.Sprintf("Hello from skopos: %s", input)), nil
		},
	)
}
