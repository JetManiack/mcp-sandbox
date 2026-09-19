// Package sandboxop performs one file operation inside a sandbox, in a child
// process running as that agent's own user.
//
// It exists because per-agent user ids take the sandbox away from the worker.
// Once a directory belongs to uid 20001 and the worker holds no
// CAP_DAC_OVERRIDE, the worker genuinely cannot read it — which is the point,
// and which means reading a file has to happen the same way running a command
// does: fork, drop to the agent's id, then act.
//
// The alternative was giving the worker DAC_OVERRIDE so it could keep doing file
// operations itself. That would make the process holding the pool's credentials
// able to read every agent's files, which is the property being removed.
package sandboxop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/procexec"
)

// Op names a file operation.
type Op string

const (
	OpRead   Op = "read"
	OpWrite  Op = "write"
	OpList   Op = "list"
	OpDelete Op = "delete"
	OpStat   Op = "stat"

	// OpLook resolves a program name against PATH.
	//
	// It is here for the same reason the file operations are: a virtual
	// environment lives inside the agent's sandbox, so the directory holding its
	// interpreter is one the worker cannot read. Resolving in the worker finds
	// nothing and reports the program as missing, which is a confusing way to say
	// "I am not allowed to look".
	OpLook Op = "look"
)

// Request is one operation to perform.
//
// Name is already resolved to a path relative to Root by the caller, which owns
// the sandbox's containment rules. The child re-enforces it regardless by working
// through os.Root, so a wrong name is refused by the kernel rather than trusted.
type Request struct {
	Root    string `json:"root"`
	Op      Op     `json:"op"`
	Name    string `json:"name"`
	Content []byte `json:"content,omitempty"`
	Perm    uint32 `json:"perm,omitempty"`

	// PathEnv is the PATH the child searches for OpLook. Passed rather than
	// inherited, because the child is given an empty environment.
	PathEnv string `json:"path_env,omitempty"`
}

// FileInfo is one directory entry on the wire between the two processes.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// Response is what the operation produced. Error is the operation's own failure,
// carried as text because it crosses a process boundary.
type Response struct {
	Error        string     `json:"error,omitempty"`
	Content      []byte     `json:"content,omitempty"`
	Files        []FileInfo `json:"files,omitempty"`
	Existed      bool       `json:"existed,omitempty"`
	WasDirectory bool       `json:"was_directory,omitempty"`
	Info         *FileInfo  `json:"info,omitempty"`

	// Program is the resolved absolute path, for OpLook.
	Program string `json:"program,omitempty"`
}

// Err returns the operation's failure, if any.
func (r Response) Err() error {
	if r.Error == "" {
		return nil
	}
	return errors.New(r.Error)
}

// Perform carries out req in the current process, which is expected to already be
// running as the agent's user.
func Perform(req Request) Response {
	// Resolved before the sandbox is opened, because a lookup is about PATH rather
	// than about the sandbox — and PATH may point at directories outside it.
	if req.Op == OpLook {
		program, err := exec.LookPath(req.Name)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Program: program}
	}

	root, err := os.OpenRoot(req.Root)
	if err != nil {
		return Response{Error: err.Error()}
	}
	defer func() { _ = root.Close() }()

	switch req.Op {
	case OpRead:
		data, err := root.ReadFile(req.Name)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Content: data}

	case OpWrite:
		perm := os.FileMode(req.Perm)
		if perm == 0 {
			perm = 0o600
		}
		if dir := filepath.Dir(req.Name); dir != "." {
			if err := root.MkdirAll(dir, 0o700); err != nil {
				return Response{Error: err.Error()}
			}
		}
		if err := root.WriteFile(req.Name, req.Content, perm); err != nil {
			return Response{Error: err.Error()}
		}
		return Response{}

	case OpList:
		return list(root, req.Name)

	case OpDelete:
		var out Response
		// Lstat, not Stat: a symlink is deleted as a link, so what the entry is
		// matters rather than what it points at.
		if info, statErr := root.Lstat(req.Name); statErr == nil {
			out.Existed = true
			out.WasDirectory = info.IsDir()
		}
		if err := root.RemoveAll(req.Name); err != nil {
			return Response{Error: err.Error()}
		}
		return out

	case OpStat:
		info, err := root.Stat(req.Name)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{Info: &FileInfo{
			Name:    filepath.Base(req.Name),
			Path:    req.Name,
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		}}

	default:
		return Response{Error: fmt.Sprintf("unknown sandbox operation %q", req.Op)}
	}
}

func list(root *os.Root, name string) Response {
	dir, err := root.Open(name)
	if err != nil {
		return Response{Error: err.Error()}
	}
	defer func() { _ = dir.Close() }()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return Response{Error: err.Error()}
	}

	files := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			// Removed between ReadDir and Info; simply no longer in the listing.
			continue
		}
		path := entry.Name()
		if name != "." {
			path = filepath.Join(name, entry.Name())
		}
		files = append(files, FileInfo{
			Name:    entry.Name(),
			Path:    path,
			IsDir:   entry.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return Response{Files: files}
}

// Serve reads one request, performs it, and writes one response. It is what the
// child process runs.
//
// One request per process rather than a long-lived helper: the cost is a fork
// against an operation that has already crossed a network hop from the server,
// and a process that exits cannot accumulate state an agent could influence.
func Serve(in io.Reader, out io.Writer) error {
	var req Request
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("read the operation: %w", err)
	}
	return json.NewEncoder(out).Encode(Perform(req))
}

// Runner spawns the child that performs an operation.
type Runner struct {
	// Exe and Args name the helper: this worker's own binary and whatever
	// argument selects its helper mode.
	Exe  string
	Args []string

	// Timeout bounds one operation. A file operation that hangs is holding a
	// tool call open, and the agent behind it is waiting on a socket.
	Timeout time.Duration
}

// DefaultTimeout bounds one helper invocation.
const DefaultTimeout = 30 * time.Second

// NewRunner returns a Runner invoking this process's own executable.
func NewRunner(args ...string) (*Runner, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate this executable to run sandbox operations: %w", err)
	}
	return &Runner{Exe: exe, Args: args, Timeout: DefaultTimeout}, nil
}

// Do performs req as uid, in a child process.
func (r *Runner) Do(ctx context.Context, uid uint32, req Request) (Response, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("encode the %s operation: %w", req.Op, err)
	}

	// #nosec G204 -- Exe is this process's own path and Args are compiled in;
	// nothing an agent supplies reaches the argument vector. What it does supply
	// travels as JSON on stdin.
	cmd := exec.CommandContext(ctx, r.Exe, r.Args...)
	cmd.Stdin = bytes.NewReader(payload)
	// Nothing of the worker's environment reaches the child: it holds the pool's
	// credential, and this child runs as an agent.
	cmd.Env = []string{}
	if req.PathEnv != "" {
		cmd.Env = []string{"PATH=" + req.PathEnv}
	}
	procexec.Configure(cmd)
	procexec.DropTo(cmd, uid)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// The helper's stderr, not just the exit status: "operation not permitted"
		// from a failed setuid and "no such file" from a missing sandbox are the
		// two likely causes and they need different fixes.
		detail := bytes.TrimSpace(stderr.Bytes())
		if len(detail) > 0 {
			return Response{}, fmt.Errorf("%s as uid %d: %w: %s", req.Op, uid, err, detail)
		}
		return Response{}, fmt.Errorf("%s as uid %d: %w", req.Op, uid, err)
	}

	// A decoder rather than Unmarshal, so anything the helper writes after its
	// response — a warning, a runtime message — does not turn a successful
	// operation into a parse error.
	var out Response
	if err := json.NewDecoder(&stdout).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("decode the %s result: %w", req.Op, err)
	}
	return out, nil
}
