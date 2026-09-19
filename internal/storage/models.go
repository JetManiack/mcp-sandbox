package storage

import "time"

// ActorKind distinguishes an AI agent from a human user. Both share the Actor
// table so every other table needs only a single foreign key, regardless of
// which kind of actor it points to — a SandboxBlock references the agent it
// stops and the human who stopped it through the same column type.
type ActorKind string

const (
	ActorKindAgent ActorKind = "agent"
	ActorKindHuman ActorKind = "human"
)

type Actor struct {
	ID          string    `gorm:"type:char(36);primaryKey" json:"id"`
	DisplayName string    `gorm:"not null;uniqueIndex" json:"display_name"`
	Kind        ActorKind `gorm:"type:varchar(10);not null;index" json:"kind"`
	CreatedAt   time.Time `json:"created_at"`
}

type AgentCredential struct {
	ID         string     `gorm:"type:char(36);primaryKey" json:"id"`
	ActorID    string     `gorm:"type:char(36);not null;index" json:"actor_id"`
	Label      string     `gorm:"type:text" json:"label,omitempty"`
	TokenHash  string     `gorm:"type:char(64);not null;uniqueIndex" json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// UserIdentity links a human Actor to the OIDC subject that authenticated it,
// and carries the role that subject's group membership maps to.
type UserIdentity struct {
	ActorID         string `gorm:"type:char(36);primaryKey"`
	KeycloakSubject string `gorm:"not null;uniqueIndex"`
	Role            string `gorm:"type:varchar(10);not null"`
}

// Session is a server-side OIDC login session, keyed by an opaque ID stored in
// the browser's session cookie. ExpiresAt is NOT the session's final expiry —
// it's a short checkpoint after which humanauth.OIDCProvider re-validates the
// session against Keycloak using RefreshToken (re-reading claims, so a
// role/group change propagates); the session's true maximum lifetime is bounded
// by how long Keycloak's own refresh token stays valid, which isn't tracked
// separately here.
type Session struct {
	ID           string `gorm:"type:char(64);primaryKey"` // a SHA-256 hex digest (humanauth.hashSessionID), not a UUID — 64 chars, not 36
	Subject      string `gorm:"not null;index"`
	DisplayName  string `gorm:"not null"`
	Role         string `gorm:"type:varchar(10);not null"`
	RefreshToken string `gorm:"not null"`
	ExpiresAt    time.Time
	CreatedAt    time.Time
}

// SandboxBlock is an administrator's emergency stop on one agent's sandbox: its
// running processes are killed and every subsequent tool call is refused until a
// human releases it.
//
// This is operational state, not a journal — one row per incident, answering
// exactly one question ("is this agent blocked right now"). It lives in the
// database rather than in memory because an in-memory flag would be cleared by
// any restart: a rollout, an eviction or an OOM-kill would silently release a
// block nobody decided to release, and the UI would show the sandbox running
// again with no trace of why.
//
// ReleasedAt nil means the block is active.
type SandboxBlock struct {
	ID      string `gorm:"type:char(36);primaryKey" json:"id"`
	ActorID string `gorm:"type:char(36);not null;index" json:"actor_id"`

	// ActiveKey holds ActorID while the block is active and NULL once it's
	// released. The unique index on it is what enforces "at most one active
	// block per agent" in the database, on both backends: SQLite and Postgres
	// both permit repeated NULLs in a unique index, so released rows accumulate
	// freely while a second active block is rejected outright.
	//
	// A partial unique index (`WHERE released_at IS NULL`) would express the
	// same rule more directly but is spelled differently on each backend; a
	// read-then-insert check in the repository would be a race, and one that
	// only shows up as "releasing the sandbox does nothing" long after the two
	// rows were written.
	ActiveKey *string `gorm:"type:char(36);uniqueIndex" json:"-"`

	BlockedByActorID string     `gorm:"type:char(36);not null" json:"blocked_by_actor_id"`
	BlockedByName    string     `gorm:"not null" json:"blocked_by_name"`
	Reason           string     `gorm:"type:text;not null" json:"reason"`
	BlockedAt        time.Time  `gorm:"not null" json:"blocked_at"`
	ReleasedAt       *time.Time `json:"released_at,omitempty"`
	ReleasedByName   string     `json:"released_by_name,omitempty"`

	// KilledProcesses records how many process groups the block tore down, so
	// the UI can distinguish "stopped a runaway" from "blocked an idle
	// sandbox".
	KilledProcesses int `json:"killed_processes"`
}

// AuditPhase distinguishes the two rows one action writes.
type AuditPhase string

const (
	// AuditStarted is written before the action is attempted, AuditFinished
	// after it returns.
	AuditStarted  AuditPhase = "started"
	AuditFinished AuditPhase = "finished"
)

// AuditEvent is one row of the journal: who did what, and how it went.
//
// Two rows per action rather than one, sharing a CallID. One row written on
// completion would be cheaper and would lose exactly the cases worth having a
// journal for: a command that hung until the pod was killed, or a process that
// died mid-call, leaves no row at all and reads as an action that never
// happened. A started row with no finished row is a question worth asking.
//
// This is a journal, not the terminal. It records that a file was written and
// how many bytes; the contents are the stream's business, and putting them here
// would turn the database into a log store with none of the retention a log
// store has.
type AuditEvent struct {
	ID string `gorm:"type:char(36);primaryKey" json:"id"`

	// CallID ties the started and finished rows of one action together.
	CallID string `gorm:"type:char(36);not null;index" json:"call_id"`

	Phase AuditPhase `gorm:"type:varchar(10);not null" json:"phase"`

	// At is indexed because every query against this table is bounded by time:
	// the UI asks for a window, and the pruner deletes by age.
	At time.Time `gorm:"not null;index" json:"at"`

	// ActorID is indexed for "what did this agent do", the other question the
	// table exists to answer. Name and kind are denormalised so the journal
	// still reads correctly after an actor is deleted — a row that says only
	// "actor 4f2e..." is a record of nothing once the actor is gone.
	ActorID   string    `gorm:"type:char(36);not null;index" json:"actor_id"`
	ActorName string    `gorm:"not null" json:"actor_name"`
	ActorKind ActorKind `gorm:"type:varchar(10);not null" json:"actor_kind"`

	// Action is the tool or operation name: exec_command, write_file,
	// block_sandbox, issue_token.
	Action string `gorm:"type:varchar(40);not null;index" json:"action"`

	// Target is what the action was aimed at — a path, or a program and its
	// arguments. Truncated on write rather than stored whole: an argument vector
	// can be enormous, and a journal that a single call can bloat is one an
	// operator eventually turns off.
	Target string `gorm:"type:text" json:"target,omitempty"`

	// WorkerID names the worker that served the call, empty when the action
	// never reached one — including when it was refused because none was
	// connected.
	WorkerID string `json:"worker_id,omitempty"`

	// ExecID ties this to the terminal stream's events for the same command, so
	// a journal entry leads to the output it produced while that is still
	// retained.
	ExecID string `gorm:"type:char(36)" json:"exec_id,omitempty"`

	// ExitCode is what the command returned, on the finished row of an exec.
	// Nullable because most actions do not have one, and a zero would read as
	// success for every file operation ever recorded.
	ExitCode *int `json:"exit_code,omitempty"`

	// Outcome and Error are set on the finished row only.
	Outcome    string `gorm:"type:varchar(10)" json:"outcome,omitempty"`
	Error      string `gorm:"type:text" json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`

	// Bytes is the size the action moved: output returned, file written or read.
	Bytes int `json:"bytes,omitempty"`
}

// Outcomes an AuditEvent's finished row can record.
//
// There is deliberately no "denied": authentication happens before a tool
// handler is reached, so a call with a bad token never gets as far as the
// journal. A name with no producer reads as a gap in the data rather than as an
// outcome that cannot occur.
const (
	AuditOutcomeOK      = "ok"
	AuditOutcomeError   = "error"
	AuditOutcomeBlocked = "blocked"

	// AuditOutcomeTimeout and AuditOutcomeStopped are commands the service cut
	// short rather than commands that failed. They were filed as "ok" until a
	// deployment review pointed out what that costs: a timeout kill is an action
	// the service took against an agent, this journal is the only record of it,
	// and "ok" is the one word that hides it.
	AuditOutcomeTimeout = "timeout"
	AuditOutcomeStopped = "stopped"
)
