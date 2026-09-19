package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"file path relative to sandbox root"`
	Content string `json:"content" jsonschema:"text content to write to the file"`
}

type WriteFileOutput struct {
	Path  string `json:"path" jsonschema:"the path that was written, as given"`
	Bytes int    `json:"bytes" jsonschema:"number of bytes written"`
}

func writeFileHandler(r *Registrar) mcp.ToolHandlerFor[WriteFileInput, WriteFileOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
		if in.Path == "" {
			return nil, WriteFileOutput{}, errors.New("path cannot be empty")
		}
		agentID, err := agentForCall(ctx, r.db)
		if err != nil {
			return nil, WriteFileOutput{}, err
		}
		written, err := r.executor.WriteFile(ctx, agentID, in.Path, in.Content)
		if err != nil {
			return nil, WriteFileOutput{}, fmt.Errorf("write file: %w", err)
		}
		return nil, WriteFileOutput{Path: in.Path, Bytes: written}, nil
	}
}

func (o WriteFileOutput) auditBytes() int { return o.Bytes }
