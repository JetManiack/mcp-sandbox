package workerhub

import (
	"context"
	"errors"
	"fmt"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

// The methods below are what the MCP tools and the REST API call. Each is one
// request to the worker pinned to that agent.
//
// Exec's error kinds are translated back into this process's sentinel errors, so
// callers keep telling a timeout from an operator's stop exactly as they did when
// execution was in-process. Losing that distinction across the wire would make a
// stopped sandbox report that the command ran out of time.

func (h *Hub) Exec(ctx context.Context, agentID string, in workerproto.ExecRequest) (workerproto.ExecResponse, error) {
	var out workerproto.ExecResponse
	if err := h.call(ctx, agentID, workerproto.OpExec, in, &out); err != nil {
		return out, err
	}
	switch out.ErrorKind {
	case workerproto.ErrorKindTimeout:
		return out, sandbox.ErrCommandTimeout
	case workerproto.ErrorKindStopped:
		return out, sandbox.ErrCommandStopped
	}
	return out, nil
}

func (h *Hub) ReadFile(ctx context.Context, agentID, path string) (string, error) {
	var out workerproto.ReadFileResponse
	err := h.call(ctx, agentID, workerproto.OpReadFile, workerproto.ReadFileRequest{Path: path}, &out)
	return out.Content, err
}

func (h *Hub) WriteFile(ctx context.Context, agentID, path, content string) (int, error) {
	// Checked against the limit the worker declared, before encoding: an agent
	// that asked to write something too large is told which limit it hit and how
	// big the file was, rather than that some payload did not fit.
	w, err := h.workerFor(agentID)
	if err != nil {
		return 0, fmt.Errorf("%w for sandbox %s", err, agentID)
	}
	if len(content) > w.limits.MaxFileBytes {
		return 0, workerproto.ErrFileTooLarge(path, len(content), w.limits.MaxFileBytes)
	}

	var out workerproto.WriteFileResponse
	err = h.call(ctx, agentID, workerproto.OpWriteFile, workerproto.WriteFileRequest{Path: path, Content: content}, &out)
	return out.Bytes, err
}

func (h *Hub) ListDir(ctx context.Context, agentID, path string) ([]workerproto.FileInfo, error) {
	var out workerproto.ListDirResponse
	if err := h.call(ctx, agentID, workerproto.OpListDir, workerproto.ListDirRequest{Path: path}, &out); err != nil {
		return nil, err
	}
	if out.Files == nil {
		out.Files = []workerproto.FileInfo{}
	}
	return out.Files, nil
}

func (h *Hub) DeleteFile(ctx context.Context, agentID, path string) (workerproto.DeleteFileResponse, error) {
	var out workerproto.DeleteFileResponse
	err := h.call(ctx, agentID, workerproto.OpDeleteFile, workerproto.DeleteFileRequest{Path: path}, &out)
	return out, err
}

func (h *Hub) Status(ctx context.Context, agentID string) (workerproto.StatusResponse, error) {
	var out workerproto.StatusResponse
	err := h.call(ctx, agentID, workerproto.OpStatus, workerproto.StatusRequest{}, &out)
	return out, err
}

// Kill tears down everything running in an agent's sandbox.
//
// An agent with no worker is not an error: it has nothing running, which is the
// state the caller asked for. Blocking such an agent is still meaningful — the
// block is what refuses its next call, wherever it lands.
func (h *Hub) Kill(ctx context.Context, agentID, byActor, reason string) (int, error) {
	var out workerproto.KillResponse
	err := h.call(ctx, agentID, workerproto.OpKill, workerproto.KillRequest{ByActor: byActor, Reason: reason}, &out)
	if errors.Is(err, ErrNoWorker) {
		return 0, nil
	}
	return out.Killed, err
}
