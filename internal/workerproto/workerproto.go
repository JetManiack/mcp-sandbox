// Package workerproto is the wire vocabulary between the server and its
// execution workers.
//
// JSON over one WebSocket per worker, rather than gRPC: coder/websocket is
// already a dependency, the message set is small, and Go structs express the
// contract without adding protoc to the build. Both sides import this package, so
// a change that breaks one fails to compile the other.
package workerproto

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/stream"
)

// Path is where workers dial in on the server.
const Path = "/worker"

// ProtocolVersion is the frame vocabulary this build speaks. Bump it whenever a
// change would make an older peer misread a frame — a new op, a renamed field, a
// different meaning for an existing one.
//
// It exists because the two ends update independently. An image-updating
// controller rolls the server and the workers as separate Deployments, so a
// window where they run different builds is routine rather than exceptional. The
// alternative to checking is a worker that connects, looks healthy, and answers a
// tool call with a field the server no longer reads — a failure that surfaces as
// wrong output rather than as a mismatch.
const ProtocolVersion = 1

// Limits are the sizes a worker will produce and accept, declared in its hello.
//
// They are on the wire rather than configured twice because the connection's read
// limit has to match them, and the two ends configure nothing in common: the
// worker knows its caps, the server owns the socket. A worker that declares its
// limits lets the server size the connection to exactly them, so the two cannot
// disagree — and a pool of workers with different caps stops being a
// misconfiguration.
type Limits struct {
	// MaxOutputBytes caps stdout and stderr separately in one exec result.
	MaxOutputBytes int `json:"max_output_bytes"`

	// MaxFileBytes caps the content of one read_file or write_file.
	MaxFileBytes int `json:"max_file_bytes"`
}

const (
	// escapeAllowance is how much JSON escaping is assumed to inflate a payload.
	//
	// A byte below 0x20 becomes `\u00XX`, six bytes, so the true worst case is
	// far higher than this — but sizing the socket for a file of nothing but
	// control characters would mean a read limit twelve times larger than
	// anything real, and the memory that implies. Two covers text and ordinary
	// binary; beyond it the payload check refuses one call, which is a graceful
	// failure rather than a dropped connection.
	escapeAllowance = 2

	// envelopeBytes is room for the frame's own fields around the payload.
	envelopeBytes = 64 << 10

	// HelloFrameBytes is the read limit in force until a worker has said hello:
	// enough for the hello frame and nothing else, since until it arrives there
	// is no basis for a larger one.
	HelloFrameBytes = 8 << 10

	// MaxNegotiatedFrameBytes is the largest read limit a worker can talk the
	// server into. Negotiation without a ceiling would let a worker declare a
	// limit of gigabytes and then send one, which is a memory exhaustion the
	// server would perform on itself.
	MaxNegotiatedFrameBytes = 64 << 20
)

// FrameBytes is the read limit a connection to a worker with these limits needs:
// the largest single payload it can produce, plus escaping and the envelope.
//
// An exec result carries stdout and stderr together, which is why the output cap
// counts twice.
func (l Limits) FrameBytes() int {
	payload := max(2*l.MaxOutputBytes, l.MaxFileBytes)
	return escapeAllowance*payload + envelopeBytes
}

// MaxPayloadBytes is the largest payload these limits can legitimately produce
// once encoded — what a frame may carry, as opposed to what it must reserve.
func (l Limits) MaxPayloadBytes() int {
	return l.FrameBytes() - envelopeBytes
}

// Validate reports limits that cannot work, so a bad configuration is refused at
// the handshake rather than on the first command that trips it.
func (l Limits) Validate() error {
	if l.MaxOutputBytes <= 0 {
		return fmt.Errorf("max output bytes is %d, which would return no command output at all", l.MaxOutputBytes)
	}
	if l.MaxFileBytes <= 0 {
		return fmt.Errorf("max file bytes is %d, which would refuse every file operation", l.MaxFileBytes)
	}
	if got := l.FrameBytes(); got > MaxNegotiatedFrameBytes {
		return fmt.Errorf("limits need a %d-byte frame, over the %d-byte ceiling", got, MaxNegotiatedFrameBytes)
	}
	return nil
}

// ErrPayloadTooLarge describes a payload that will not fit in one frame. Both
// ends produce it, so an agent gets the same sentence whichever side noticed.
//
// It is the backstop rather than the first line: sizes that can be known in
// advance — a file's, a configured cap — are refused by name before anything is
// encoded. This catches what is left, notably output that escaping inflated past
// what the socket was sized for.
func ErrPayloadTooLarge(op Op, size, limit int) error {
	return fmt.Errorf("%s payload is %d bytes, over the %d-byte limit one message may carry",
		op, size, limit)
}

// ErrFileTooLarge names the file cap, which is a configured limit an operator can
// raise — unlike a payload that merely did not fit, which reads as a defect.
func ErrFileTooLarge(path string, size, limit int) error {
	return fmt.Errorf("file %s is %d bytes, over this worker's %d-byte limit for one file",
		path, size, limit)
}

// FrameType discriminates what a frame carries.
type FrameType string

const (
	// FrameHello is the worker's first frame, announcing itself.
	FrameHello FrameType = "hello"

	// FrameRequest asks the worker to perform one operation.
	FrameRequest FrameType = "request"

	// FrameCancel abandons an in-flight request: the calling agent went away, or
	// an operator stopped the sandbox. The worker cancels that operation, which
	// tears down its process group.
	FrameCancel FrameType = "cancel"

	// FrameResult answers exactly one request.
	FrameResult FrameType = "result"

	// FrameEvent carries sandbox output. Events are not answers to a request —
	// they arrive while one is still running — so they are their own frame type.
	FrameEvent FrameType = "event"
)

// Op names an operation the server can ask a worker to perform. They mirror the
// MCP tools one for one, plus the kill an operator's stop needs.
type Op string

const (
	OpExec       Op = "exec"
	OpReadFile   Op = "read_file"
	OpWriteFile  Op = "write_file"
	OpListDir    Op = "list_dir"
	OpDeleteFile Op = "delete_file"
	OpStatus     Op = "status"
	OpKill       Op = "kill"
)

// Frame is every message on the wire. One envelope rather than several keeps the
// reader a single switch, and the unused fields cost nothing on the wire because
// they are all omitempty.
type Frame struct {
	Type FrameType `json:"type"`

	// ID correlates a request with its result, and names the request a cancel
	// abandons. Assigned by the server, unique per connection.
	ID uint64 `json:"id,omitempty"`

	// Hello.
	WorkerID string  `json:"worker_id,omitempty"`
	Version  string  `json:"version,omitempty"`
	Limits   *Limits `json:"limits,omitempty"`

	// Protocol is the frame vocabulary the worker speaks. Distinct from Version,
	// which is the build stamp: two builds of different ages speak the same
	// protocol far more often than not, and refusing on the build string would
	// make every rollout an outage.
	Protocol int `json:"protocol,omitempty"`

	// Agents are the sandboxes this worker already holds, sent on reconnect so
	// the server can pin them back where their files are.
	//
	// Without it a server restart scatters every agent onto whichever worker is
	// least loaded, handing each an empty sandbox while the old one sits on disk
	// a few pods away — the work is not lost so much as no longer addressable.
	Agents []string `json:"agents,omitempty"`

	// Request: which agent's sandbox, which operation, and its arguments.
	AgentID string          `json:"agent_id,omitempty"`
	Op      Op              `json:"op,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`

	// Result.
	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`

	// Event.
	Event *stream.Event `json:"event,omitempty"`
}

// ExecRequest runs a command in an agent's sandbox. It mirrors the exec_command
// tool: with Args set the program is executed directly, and TimeoutSec zero means
// the worker's default.
type ExecRequest struct {
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
	WorkDir    string   `json:"work_dir,omitempty"`
}

// ExecResponse is what the command produced.
//
// ErrorKind carries the difference between a timeout, an operator's stop and an
// ordinary failure across the wire: an error string alone would lose it, and the
// server has to reproduce the same distinction its callers already rely on.
type ExecResponse struct {
	ExecID     string `json:"exec_id"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
	ErrorKind  string `json:"error_kind,omitempty"`
}

// Error kinds an ExecResponse can carry.
const (
	ErrorKindTimeout = "timeout"
	ErrorKindStopped = "stopped"
)

type ReadFileRequest struct {
	Path string `json:"path"`
}

type ReadFileResponse struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type WriteFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type WriteFileResponse struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

type ListDirRequest struct {
	Path string `json:"path"`
}

type ListDirResponse struct {
	Path  string     `json:"path"`
	Files []FileInfo `json:"files"`
}

// FileInfo mirrors sandbox.FileInfo on the wire.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

type DeleteFileRequest struct {
	Path string `json:"path"`
}

type DeleteFileResponse struct {
	Path         string `json:"path"`
	Existed      bool   `json:"existed"`
	WasDirectory bool   `json:"was_directory"`
}

type StatusRequest struct{}

// StatusResponse mirrors sandbox.SandboxStatus, plus the worker that answered —
// which is the one thing an operator debugging a scaled-out pool always wants and
// cannot get anywhere else.
type StatusResponse struct {
	RootDir         string   `json:"root_dir"`
	DefaultTimeout  string   `json:"default_timeout"`
	MaxOutputBytes  int      `json:"max_output_bytes"`
	EnvNames        []string `json:"env_names"`
	RunningCommands int      `json:"running_commands"`
	WorkerID        string   `json:"worker_id"`
}

// KillRequest tears down everything running in an agent's sandbox.
type KillRequest struct {
	ByActor string `json:"by_actor"`
	Reason  string `json:"reason"`
}

type KillResponse struct {
	Killed int `json:"killed"`
}

// Marshal encodes a payload for a frame, panicking only on a type that cannot be
// JSON at all — which is a programming error in this package, not a runtime
// condition.
func Marshal(payload any) (json.RawMessage, error) {
	return json.Marshal(payload)
}

// Unmarshal decodes a frame payload.
func Unmarshal[T any](raw json.RawMessage, out *T) error {
	return json.Unmarshal(raw, out)
}
