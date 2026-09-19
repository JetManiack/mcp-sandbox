package localmcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
)

// The schemas below mirror the server's tools of the same names, so a client can
// be pointed at either without changing how it calls them. What differs is stated
// in each description rather than left for a reader to infer: paths here are
// relative to the helper's directory and are not confined to it, because this
// helper confines nothing.

type ReadFileInput struct {
	Path string `json:"path" jsonschema:"file path, relative to the helper's directory or absolute"`
}

type ReadFileOutput struct {
	Path    string `json:"path" jsonschema:"the path that was read, as given"`
	Content string `json:"content" jsonschema:"the file's contents"`

	// The server's read_file has no such field, and cannot truncate. Here the cap
	// exists because the contents travel back inside one MCP response, and an agent
	// has to be able to tell a whole file from the first megabyte of one.
	Truncated bool `json:"truncated" jsonschema:"true when the file was longer than the output cap and was cut"`
}

func readFileHandler(runner *localexec.Runner) mcp.ToolHandlerFor[ReadFileInput, ReadFileOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ReadFileInput) (*mcp.CallToolResult, ReadFileOutput, error) {
		if in.Path == "" {
			return nil, ReadFileOutput{}, errors.New("path cannot be empty")
		}
		content, truncated, err := runner.ReadFile(in.Path)
		if err != nil {
			return nil, ReadFileOutput{}, fmt.Errorf("read file: %w", err)
		}
		return nil, ReadFileOutput{Path: in.Path, Content: content, Truncated: truncated}, nil
	}
}

type WriteFileInput struct {
	Path    string `json:"path" jsonschema:"file path, relative to the helper's directory or absolute"`
	Content string `json:"content" jsonschema:"text content to write to the file"`
}

type WriteFileOutput struct {
	Path  string `json:"path" jsonschema:"the path that was written, as given"`
	Bytes int    `json:"bytes" jsonschema:"number of bytes written"`
}

func writeFileHandler(runner *localexec.Runner) mcp.ToolHandlerFor[WriteFileInput, WriteFileOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in WriteFileInput) (*mcp.CallToolResult, WriteFileOutput, error) {
		if in.Path == "" {
			return nil, WriteFileOutput{}, errors.New("path cannot be empty")
		}
		data := []byte(in.Content)
		if err := runner.WriteFile(in.Path, data); err != nil {
			return nil, WriteFileOutput{}, fmt.Errorf("write file: %w", err)
		}
		return nil, WriteFileOutput{Path: in.Path, Bytes: len(data)}, nil
	}
}

type ListDirInput struct {
	Path string `json:"path,omitempty" jsonschema:"directory path, relative to the helper's directory or absolute; defaults to the helper's directory"`
}

type ListDirOutput struct {
	Path  string               `json:"path" jsonschema:"the directory that was listed"`
	Files []localexec.FileInfo `json:"files" jsonschema:"entries in the directory, non-recursive"`
}

func listDirHandler(runner *localexec.Runner) mcp.ToolHandlerFor[ListDirInput, ListDirOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ListDirInput) (*mcp.CallToolResult, ListDirOutput, error) {
		path := in.Path
		if path == "" {
			path = "."
		}
		files, err := runner.ListDir(path)
		if err != nil {
			return nil, ListDirOutput{}, fmt.Errorf("list directory: %w", err)
		}
		// An empty directory must serialize as [] rather than null: a client
		// distinguishing "no entries" from "field absent" would otherwise see the
		// latter.
		if files == nil {
			files = []localexec.FileInfo{}
		}
		return nil, ListDirOutput{Path: path, Files: files}, nil
	}
}

type DeleteFileInput struct {
	Path string `json:"path" jsonschema:"file or directory path, relative to the helper's directory or absolute; directories are removed recursively"`
}

type DeleteFileOutput struct {
	Path         string `json:"path" jsonschema:"the path that was deleted, as given"`
	Existed      bool   `json:"existed" jsonschema:"false when there was nothing at that path to delete"`
	WasDirectory bool   `json:"was_directory" jsonschema:"true when the path was a directory and its whole subtree was removed"`
}

func deleteFileHandler(runner *localexec.Runner) mcp.ToolHandlerFor[DeleteFileInput, DeleteFileOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in DeleteFileInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
		if in.Path == "" {
			return nil, DeleteFileOutput{}, errors.New("path cannot be empty")
		}
		result, err := runner.DeleteFile(in.Path)
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

type SandboxStatusInput struct{}

type SandboxStatusOutput struct {
	// Named status to match the server's tool, whose output nests the same way.
	// The name says "sandbox" and this helper has none — which is exactly why
	// Status.Sandboxed is reported: an agent that assumes otherwise from the tool
	// name will take risks it would not otherwise take.
	Status localexec.Status `json:"status" jsonschema:"the helper's working directory, shell and limits — and that nothing is sandboxed"`
}

func sandboxStatusHandler(runner *localexec.Runner) mcp.ToolHandlerFor[SandboxStatusInput, SandboxStatusOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in SandboxStatusInput) (*mcp.CallToolResult, SandboxStatusOutput, error) {
		return nil, SandboxStatusOutput{Status: runner.Status()}, nil
	}
}
