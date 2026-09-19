package workerlink

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

// dispatch performs one operation against the agent's sandbox on this worker.
//
// Sandboxes are created on first use, exactly as they were when execution lived in
// the server: a worker serves many agents, so an HPA can scale the pool without
// anything above it knowing which pod holds which sandbox.
func (l *Link) dispatch(ctx context.Context, frame workerproto.Frame) (any, error) {
	if frame.AgentID == "" {
		return nil, errors.New("request carries no agent id")
	}
	sb, err := l.manager.GetSandbox(frame.AgentID)
	if err != nil {
		return nil, err
	}

	switch frame.Op {
	case workerproto.OpExec:
		return l.exec(ctx, sb, frame)
	case workerproto.OpReadFile:
		var in workerproto.ReadFileRequest
		if err := workerproto.Unmarshal(frame.Payload, &in); err != nil {
			return nil, err
		}
		// Checked by stat before reading: a file that cannot be returned should
		// not be loaded into memory first, or a worker serving several agents
		// balloons by the size of a file nobody will receive.
		if info, err := sb.Stat(in.Path); err == nil && info.Size > int64(l.limits.MaxFileBytes) {
			return nil, workerproto.ErrFileTooLarge(in.Path, int(info.Size), l.limits.MaxFileBytes)
		}
		data, err := sb.ReadFile(in.Path)
		if err != nil {
			return nil, err
		}
		return workerproto.ReadFileResponse{Path: in.Path, Content: string(data)}, nil

	case workerproto.OpWriteFile:
		var in workerproto.WriteFileRequest
		if err := workerproto.Unmarshal(frame.Payload, &in); err != nil {
			return nil, err
		}
		data := []byte(in.Content)
		// Belt and braces: the server refuses this before dispatch, using the
		// limits from this worker's hello. A worker that trusted that would be
		// trusting the other end to enforce its own cap.
		if len(data) > l.limits.MaxFileBytes {
			return nil, workerproto.ErrFileTooLarge(in.Path, len(data), l.limits.MaxFileBytes)
		}
		if err := sb.WriteFile(in.Path, data, 0o644); err != nil {
			return nil, err
		}
		return workerproto.WriteFileResponse{Path: in.Path, Bytes: len(data)}, nil

	case workerproto.OpListDir:
		var in workerproto.ListDirRequest
		if err := workerproto.Unmarshal(frame.Payload, &in); err != nil {
			return nil, err
		}
		files, err := sb.ListDir(in.Path)
		if err != nil {
			return nil, err
		}
		wire := make([]workerproto.FileInfo, 0, len(files))
		for _, file := range files {
			wire = append(wire, workerproto.FileInfo{
				Name:    file.Name,
				Path:    file.Path,
				IsDir:   file.IsDir,
				Size:    file.Size,
				ModTime: file.ModTime,
			})
		}
		return workerproto.ListDirResponse{Path: in.Path, Files: wire}, nil

	case workerproto.OpDeleteFile:
		var in workerproto.DeleteFileRequest
		if err := workerproto.Unmarshal(frame.Payload, &in); err != nil {
			return nil, err
		}
		result, err := sb.DeleteFile(in.Path)
		if err != nil {
			return nil, err
		}
		return workerproto.DeleteFileResponse{
			Path:         in.Path,
			Existed:      result.Existed,
			WasDirectory: result.WasDirectory,
		}, nil

	case workerproto.OpStatus:
		status := sb.GetStatus()
		return workerproto.StatusResponse{
			RootDir:         status.RootDir,
			DefaultTimeout:  status.DefaultTimeout,
			MaxOutputBytes:  status.MaxOutputBytes,
			EnvNames:        status.EnvNames,
			RunningCommands: status.RunningCommand,
			WorkerID:        l.cfg.WorkerID,
		}, nil

	case workerproto.OpKill:
		var in workerproto.KillRequest
		if err := workerproto.Unmarshal(frame.Payload, &in); err != nil {
			return nil, err
		}
		killed, err := sb.KillAll(in.ByActor, in.Reason)
		if err != nil {
			return nil, err
		}
		return workerproto.KillResponse{Killed: killed}, nil

	default:
		return nil, fmt.Errorf("unknown operation %q", frame.Op)
	}
}

// exec runs a command and reports how it ended.
//
// A timeout or a stop comes back as a successful result carrying an error kind
// rather than a failed request, because the output the command produced first is
// worth keeping — for a hung build it is the only useful thing. The server turns
// the kind back into the same sentinel error its callers had before execution
// moved out of process.
func (l *Link) exec(ctx context.Context, sb *sandbox.Sandbox, frame workerproto.Frame) (any, error) {
	var in workerproto.ExecRequest
	if err := workerproto.Unmarshal(frame.Payload, &in); err != nil {
		return nil, err
	}

	var timeout time.Duration
	if in.TimeoutSec > 0 {
		timeout = time.Duration(in.TimeoutSec) * time.Second
	}

	// The result is read even when execErr is set, and that is the point rather
	// than an oversight: a command cut short by its timeout or by an operator's
	// stop has usually produced output already, and the whole value of a timeout
	// is seeing what the command managed to say before it hit one. ExecResult is a
	// value, so there is nothing to dereference — the fields are simply what was
	// collected before the error.
	res, execErr := sb.ExecCommand(ctx, in.Command, in.Args, timeout, in.WorkDir)
	out := workerproto.ExecResponse{
		ExecID:     res.ExecID,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		ExitCode:   res.ExitCode,
		DurationMs: res.DurationMs,
		Truncated:  res.Truncated,
	}

	switch {
	case errors.Is(execErr, sandbox.ErrCommandTimeout):
		out.ErrorKind = workerproto.ErrorKindTimeout
	case errors.Is(execErr, sandbox.ErrCommandStopped):
		out.ErrorKind = workerproto.ErrorKindStopped
	case execErr != nil:
		// It never ran — a missing program, an unusable working directory — so
		// there is no output to preserve and the request simply failed.
		return nil, execErr
	}
	return out, nil
}
