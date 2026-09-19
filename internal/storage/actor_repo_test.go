package storage_test

import (
	"errors"
	"testing"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

func TestAgentTokenLifecycle(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "TestAgent")
		if agent.Kind != storage.ActorKindAgent {
			t.Errorf("kind = %q, want %q", agent.Kind, storage.ActorKindAgent)
		}

		rawToken, cred, err := storage.IssueAgentToken(db, agent.ID)
		if err != nil {
			t.Fatalf("IssueAgentToken: %v", err)
		}
		if cred.ActorID != agent.ID {
			t.Errorf("credential actor = %q, want %q", cred.ActorID, agent.ID)
		}

		authenticated, err := storage.AuthenticateAgentToken(db, rawToken)
		if err != nil {
			t.Fatalf("AuthenticateAgentToken: %v", err)
		}
		if authenticated.ID != agent.ID {
			t.Errorf("authenticated actor = %q, want %q", authenticated.ID, agent.ID)
		}

		if err := storage.RevokeAgentToken(db, cred.ID); err != nil {
			t.Fatalf("RevokeAgentToken: %v", err)
		}
		if _, err := storage.AuthenticateAgentToken(db, rawToken); !errors.Is(err, storage.ErrTokenRevoked) {
			t.Errorf("error after revoke = %v, want ErrTokenRevoked", err)
		}
	})
}

// TestTokenIsStoredHashedOnly is the check that keeps a database read from
// yielding usable credentials.
func TestTokenIsStoredHashedOnly(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "TestAgent")
		rawToken, cred, err := storage.IssueAgentToken(db, agent.ID)
		if err != nil {
			t.Fatalf("IssueAgentToken: %v", err)
		}

		var stored storage.AgentCredential
		if err := db.First(&stored, "id = ?", cred.ID).Error; err != nil {
			t.Fatalf("reload credential: %v", err)
		}
		if stored.TokenHash == rawToken {
			t.Fatal("credential stores the raw token")
		}
		if stored.TokenHash != storage.HashTokenForTesting(rawToken) {
			t.Errorf("stored hash does not match hash of the issued token")
		}
	})
}

func TestIssueTokenForUnknownActor(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		if _, _, err := storage.IssueAgentToken(db, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, storage.ErrActorNotFound) {
			t.Errorf("error = %v, want ErrActorNotFound", err)
		}
	})
}

func TestCreateAgentValidation(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		if _, err := storage.CreateAgent(db, "   "); !errors.Is(err, storage.ErrEmptyDisplayName) {
			t.Errorf("blank name error = %v, want ErrEmptyDisplayName", err)
		}

		mustAgent(t, db, "duplicate")
		if _, err := storage.CreateAgent(db, "duplicate"); !errors.Is(err, storage.ErrDisplayNameConflict) {
			t.Errorf("duplicate name error = %v, want ErrDisplayNameConflict", err)
		}
	})
}

func TestActorsWithActiveToken(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		withToken := mustAgent(t, db, "has-token")
		revoked := mustAgent(t, db, "revoked")
		never := mustAgent(t, db, "never-issued")

		if _, _, err := storage.IssueAgentToken(db, withToken.ID); err != nil {
			t.Fatalf("IssueAgentToken: %v", err)
		}
		if _, _, err := storage.IssueAgentToken(db, revoked.ID); err != nil {
			t.Fatalf("IssueAgentToken: %v", err)
		}
		if err := storage.RevokeAllAgentCredentials(db, revoked.ID); err != nil {
			t.Fatalf("RevokeAllAgentCredentials: %v", err)
		}

		active, err := storage.ActorsWithActiveToken(db)
		if err != nil {
			t.Fatalf("ActorsWithActiveToken: %v", err)
		}
		if !active[withToken.ID] {
			t.Error("agent with a live token reported as having none")
		}
		if active[revoked.ID] {
			t.Error("agent whose only token was revoked reported as active")
		}
		if active[never.ID] {
			t.Error("agent that never had a token reported as active")
		}
	})
}

func TestGetOrCreateHumanActorUpdatesRole(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		first, err := storage.GetOrCreateHumanActor(db, "subject-1", "Ada", "viewer")
		if err != nil {
			t.Fatalf("GetOrCreateHumanActor: %v", err)
		}

		// A group change in the auth provider has to reach the database, or a
		// demoted admin keeps admin rights for as long as the row survives.
		again, err := storage.GetOrCreateHumanActor(db, "subject-1", "Ada", "admin")
		if err != nil {
			t.Fatalf("GetOrCreateHumanActor (role change): %v", err)
		}
		if again.ID != first.ID {
			t.Errorf("actor id changed on role update: %q then %q", first.ID, again.ID)
		}

		var identity storage.UserIdentity
		if err := db.First(&identity, "keycloak_subject = ?", "subject-1").Error; err != nil {
			t.Fatalf("reload identity: %v", err)
		}
		if identity.Role != "admin" {
			t.Errorf("role = %q, want %q", identity.Role, "admin")
		}
	})
}
