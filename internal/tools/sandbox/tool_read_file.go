package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ReadFileInput struct {
	Path string `json:"path" jsonschema:"file path relative to sandbox root"`
}

type ReadFileOutput struct {
	Path    string `json:"path" jsonschema:"the path that was read, as given"`
	Content string `json:"content" jsonschema:"the file's contents"`
}

func readFileHandler(r *Registrar) mcp.ToolHandlerFor[ReadFileInput, ReadFileOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
		if in.Path == "" {
			return nil, ReadFileOutput{}, errors.New("path cannot be empty")
		}
		agentID, err := agentForCall(ctx, r.db)
		if err != nil {
			return nil, ReadFileOutput{}, err
		}
		content, err := r.executor.ReadFile(ctx, agentID, in.Path)
		if err != nil {
			return nil, ReadFileOutput{}, fmt.Errorf("read file: %w", err)
		}
		return nil, ReadFileOutput{Path: in.Path, Content: content}, nil
	}
}

func (o ReadFileOutput) auditBytes() int { return len(o.Content) }
