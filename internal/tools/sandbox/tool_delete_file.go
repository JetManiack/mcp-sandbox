package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type DeleteFileInput struct {
	Path string `json:"path" jsonschema:"file or directory path relative to sandbox root; directories are removed recursively"`
}

type DeleteFileOutput struct {
	Path         string `json:"path" jsonschema:"the path that was deleted, as given"`
	Existed      bool   `json:"existed" jsonschema:"false when there was nothing at that path to delete"`
	WasDirectory bool   `json:"was_directory" jsonschema:"true when the path was a directory and its whole subtree was removed"`
}

func deleteFileHandler(r *Registrar) mcp.ToolHandlerFor[DeleteFileInput, DeleteFileOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in DeleteFileInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
		if in.Path == "" {
			return nil, DeleteFileOutput{}, errors.New("path cannot be empty")
		}
		agentID, err := agentForCall(ctx, r.db)
		if err != nil {
			return nil, DeleteFileOutput{}, err
		}
		result, err := r.executor.DeleteFile(ctx, agentID, in.Path)
		if err != nil {
			return nil, DeleteFileOutput{}, fmt.Errorf("delete: %w", err)
		}
		return nil, DeleteFileOutput{
			Path:         in.Path,
			Existed:      result.Existed,
			WasDirectory: result.WasDirectory,
		}, nil
	}
}
