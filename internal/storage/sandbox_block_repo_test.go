package storage_test

import (
	"errors"
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

func TestBlockAndReleaseCycle(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "runaway")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		if block, err := storage.ActiveSandboxBlock(db, agent.ID); err != nil || block != nil {
			t.Fatalf("fresh agent: block = %v, err = %v; want nil, nil", block, err)
		}

		block, err := storage.BlockSandbox(db, agent.ID, admin, "spawning processes in a loop")
		if err != nil {
			t.Fatalf("BlockSandbox: %v", err)
		}
		if block.BlockedByName != "Grace" || block.Reason != "spawning processes in a loop" {
			t.Errorf("block = %+v, want it to record who and why", block)
		}

		active, err := storage.ActiveSandboxBlock(db, agent.ID)
		if err != nil {
			t.Fatalf("ActiveSandboxBlock: %v", err)
		}
		if active == nil || active.ID != block.ID {
			t.Fatalf("active block = %+v, want the one just created", active)
		}

		if err := storage.ReleaseSandbox(db, agent.ID, admin); err != nil {
			t.Fatalf("ReleaseSandbox: %v", err)
		}
		released, err := storage.ActiveSandboxBlock(db, agent.ID)
		if err != nil {
			t.Fatalf("ActiveSandboxBlock after release: %v", err)
		}
		if released != nil {
			t.Errorf("block still active after release: %+v", released)
		}

		// The released row must survive: it is the record of what happened.
		var stored storage.SandboxBlock
		if err := db.First(&stored, "id = ?", block.ID).Error; err != nil {
			t.Fatalf("released block row is gone: %v", err)
		}
		if stored.ReleasedAt == nil || stored.ReleasedByName != "Grace" {
			t.Errorf("stored = %+v, want released_at and released_by set", stored)
		}
	})
}

func TestBlockRequiresReason(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "agent")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		// A block with no reason is one nobody can review afterwards.
		if _, err := storage.BlockSandbox(db, agent.ID, admin, ""); !errors.Is(err, storage.ErrEmptyReason) {
			t.Errorf("error = %v, want ErrEmptyReason", err)
		}
	})
}

func TestDoubleBlockIsRejected(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "agent")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		if _, err := storage.BlockSandbox(db, agent.ID, admin, "first"); err != nil {
			t.Fatalf("first BlockSandbox: %v", err)
		}
		if _, err := storage.BlockSandbox(db, agent.ID, admin, "second"); !errors.Is(err, storage.ErrAlreadyBlocked) {
			t.Errorf("second block error = %v, want ErrAlreadyBlocked", err)
		}
	})
}

func TestReleaseUnblockedSandbox(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "agent")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		if err := storage.ReleaseSandbox(db, agent.ID, admin); !errors.Is(err, storage.ErrNotBlocked) {
			t.Errorf("error = %v, want ErrNotBlocked", err)
		}
	})
}

// TestBlockAfterReleaseIsAllowed covers the reason ActiveKey is nullable rather
// than a plain unique column: an agent blocked, released and blocked again must
// not be permanently un-blockable by its own history.
func TestBlockAfterReleaseIsAllowed(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "agent")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		for round := range 3 {
			if _, err := storage.BlockSandbox(db, agent.ID, admin, "round"); err != nil {
				t.Fatalf("BlockSandbox round %d: %v", round, err)
			}
			if err := storage.ReleaseSandbox(db, agent.ID, admin); err != nil {
				t.Fatalf("ReleaseSandbox round %d: %v", round, err)
			}
		}

		var count int64
		if err := db.Model(&storage.SandboxBlock{}).Where("actor_id = ?", agent.ID).Count(&count).Error; err != nil {
			t.Fatalf("count blocks: %v", err)
		}
		if count != 3 {
			t.Errorf("stored blocks = %d, want 3", count)
		}
	})
}

// TestConcurrentBlocksProduceExactlyOne is the invariant the unique index
// exists for: two administrators pressing the button simultaneously must not
// leave two rows that both look active, because releasing one would then leave
// the sandbox blocked with no visible reason.
func TestConcurrentBlocksProduceExactlyOne(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "agent")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		const racers = 8
		var wg sync.WaitGroup
		results := make([]error, racers)
		start := make(chan struct{})

		for i := range racers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := storage.BlockSandbox(db, agent.ID, admin, "concurrent")
				results[i] = err
			}(i)
		}
		close(start)
		wg.Wait()

		var succeeded, alreadyBlocked, other int
		for _, err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, storage.ErrAlreadyBlocked):
				alreadyBlocked++
			default:
				other++
				t.Errorf("unexpected error: %v", err)
			}
		}

		if succeeded != 1 {
			t.Errorf("successful blocks = %d, want exactly 1 (already-blocked: %d, other: %d)", succeeded, alreadyBlocked, other)
		}

		var active int64
		if err := db.Model(&storage.SandboxBlock{}).Where("active_key IS NOT NULL").Count(&active).Error; err != nil {
			t.Fatalf("count active blocks: %v", err)
		}
		if active != 1 {
			t.Errorf("active block rows = %d, want 1", active)
		}
	})
}

func TestActiveSandboxBlocksIsKeyedByActor(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		admin := mustHuman(t, db, "admin-subject", "Grace")
		blocked := mustAgent(t, db, "blocked")
		free := mustAgent(t, db, "free")

		if _, err := storage.BlockSandbox(db, blocked.ID, admin, "why"); err != nil {
			t.Fatalf("BlockSandbox: %v", err)
		}

		byActor, err := storage.ActiveSandboxBlocks(db)
		if err != nil {
			t.Fatalf("ActiveSandboxBlocks: %v", err)
		}
		if byActor[blocked.ID] == nil {
			t.Error("blocked agent missing from the active-block map")
		}
		if byActor[free.ID] != nil {
			t.Error("unblocked agent present in the active-block map")
		}
	})
}

func TestRecordKilledProcesses(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "agent")
		admin := mustHuman(t, db, "admin-subject", "Grace")

		block, err := storage.BlockSandbox(db, agent.ID, admin, "runaway")
		if err != nil {
			t.Fatalf("BlockSandbox: %v", err)
		}
		if err := storage.RecordKilledProcesses(db, block.ID, 3); err != nil {
			t.Fatalf("RecordKilledProcesses: %v", err)
		}

		reloaded, err := storage.ActiveSandboxBlock(db, agent.ID)
		if err != nil {
			t.Fatalf("ActiveSandboxBlock: %v", err)
		}
		if reloaded.KilledProcesses != 3 {
			t.Errorf("killed processes = %d, want 3", reloaded.KilledProcesses)
		}
	})
}
