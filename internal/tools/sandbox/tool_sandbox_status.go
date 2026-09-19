package sandbox

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

type SandboxStatusInput struct{}

type SandboxStatusOutput struct {
	// The worker that answered is part of the status now: in a scaled-out pool
	// "which pod is my sandbox on" is the first question an operator asks and
	// the only place it can be answered.
	Status workerproto.StatusResponse `json:"status" jsonschema:"the calling agent's sandbox configuration, limits and the worker holding it"`
}

func sandboxStatusHandler(r *Registrar) mcp.ToolHandlerFor[SandboxStatusInput, SandboxStatusOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in SandboxStatusInput) (*mcp.CallToolResult, SandboxStatusOutput, error) {
		agentID, err := agentForCall(ctx, r.db)
		if err != nil {
			return nil, SandboxStatusOutput{}, err
		}
		status, err := r.executor.Status(ctx, agentID)
		if err != nil {
			return nil, SandboxStatusOutput{}, err
		}
		return nil, SandboxStatusOutput{Status: status}, nil
	}
}
