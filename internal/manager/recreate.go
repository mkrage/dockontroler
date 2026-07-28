package manager

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// backupSuffix is appended to the original container's name while its
// replacement is being built. It is deliberately ugly and unlikely to collide,
// and if a recreate is ever interrupted hard enough to leave one behind, the name
// says exactly where it came from.
const backupSuffix = "__dockontroler_old"

const (
	// recreateTimeout bounds the whole operation. Generous, because it covers a
	// graceful stop plus creating and starting a replacement.
	recreateTimeout = 3 * time.Minute
	// rollbackTimeout bounds the undo path.
	rollbackTimeout = 2 * time.Minute
)

// RecreateResult describes a completed recreate.
type RecreateResult struct {
	// NewID is the replacement container's id.
	NewID string
	// Notes are things worth showing the user: anonymous volumes that were
	// carried over, a digest-pinned image, a leftover that could not be removed.
	Notes []string
}

// Recreate replaces a container with an equivalent one built from the current
// state of its image tag.
//
// This is the operation that picks up a locally rebuilt image, which a restart
// cannot do: a container is bound to the image id it was created from.
//
// The original is renamed rather than deleted, and only removed once the
// replacement is running. Any failure along the way is rolled back, so the worst
// realistic outcome is "nothing changed" instead of "the container is gone".
func (m *Manager) Recreate(ctx context.Context, ref string) (RecreateResult, error) {
	inspected, release, err := m.begin(ctx, ref)
	if err != nil {
		return RecreateResult{}, err
	}
	defer release()

	if err := m.guardSelf(inspected, "recreate"); err != nil {
		return RecreateResult{}, err
	}

	plan, err := planRecreate(inspected)
	if err != nil {
		return RecreateResult{}, err
	}

	// From the rename onwards this must run to completion. If the caller's
	// context died — the browser tab was closed, the bot's poll timed out —
	// abandoning half-way would leave the container sitting under its backup
	// name with nothing serving traffic. So the Engine calls get a context of
	// their own, with their own deadline.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recreateTimeout)
	defer cancel()

	return m.executeRecreate(opCtx, inspected.ID, plan)
}

// recreateProgress records how far a recreate got, so rollback only undoes steps
// that actually happened.
type recreateProgress struct {
	stopped bool
	renamed bool
	newID   string
}

func (m *Manager) executeRecreate(ctx context.Context, oldID string, plan recreatePlan) (RecreateResult, error) {
	log := m.log.With("container", plan.Name, "id", shortID(oldID))
	log.Info("recreating container", "image", plan.Body["Image"], "was_running", plan.WasRunning)

	var progress recreateProgress

	// Step 1: stop the original. Nothing is committed yet, so a failure here
	// needs no rollback beyond what the Engine already did.
	if plan.WasRunning {
		if err := m.docker.StopContainer(ctx, oldID, m.stopTimeout); err != nil {
			return RecreateResult{}, fmt.Errorf("stop %s before recreating: %w", plan.Name, err)
		}
		progress.stopped = true
	}

	// Step 2: free the name. Everything after this point is rolled back on error.
	backupName := plan.Name + backupSuffix
	if err := m.docker.RenameContainer(ctx, oldID, backupName); err != nil {
		m.rollback(ctx, oldID, plan, progress, log)
		return RecreateResult{}, fmt.Errorf("rename %s out of the way: %w", plan.Name, err)
	}
	progress.renamed = true

	// Step 3: build the replacement.
	created, err := m.docker.CreateContainer(ctx, plan.Name, plan.Body)
	if err != nil {
		m.rollback(ctx, oldID, plan, progress, log)
		return RecreateResult{}, fmt.Errorf("create the replacement for %s: %w", plan.Name, err)
	}
	progress.newID = created.ID
	for _, warning := range created.Warnings {
		log.Warn("docker warning while creating the replacement", "warning", warning)
	}

	// Step 4: attach the remaining networks. A create call reliably attaches only
	// one, so a container on both a frontend and a database network needs this.
	for _, attachment := range plan.Extra {
		if err := m.docker.ConnectNetwork(ctx, attachment.Name, created.ID, attachment.Config); err != nil {
			m.rollback(ctx, oldID, plan, progress, log)
			return RecreateResult{}, fmt.Errorf("attach %s to network %s: %w", plan.Name, attachment.Name, err)
		}
	}

	// Step 5: start it, but only if the original had been running. Recreating a
	// stopped container should leave it stopped.
	if plan.WasRunning {
		if err := m.docker.StartContainer(ctx, created.ID); err != nil {
			m.rollback(ctx, oldID, plan, progress, log)
			return RecreateResult{}, fmt.Errorf("start the replacement for %s: %w", plan.Name, err)
		}
	}

	result := RecreateResult{NewID: created.ID, Notes: plan.Notes}

	// Step 6: the replacement is up, so the original can go.
	//
	// v=false is essential: removing with volumes would delete the anonymous
	// volumes the replacement is now using, which is how a naive recreate
	// destroys a database.
	if err := m.docker.RemoveContainer(ctx, oldID, false); err != nil {
		log.Warn("replacement is running but the old container could not be removed",
			"leftover", backupName, "error", err)
		result.Notes = append(result.Notes, fmt.Sprintf(
			"the replacement is running, but the old container could not be removed; "+
				"remove %q manually", backupName))
	}

	log.Info("recreate finished", "new_id", shortID(created.ID), "notes", len(result.Notes))
	return result, nil
}

// rollback undoes a failed recreate as far as it got: discard the half-built
// replacement, give the original its name back, and start it again if it had been
// running.
//
// It builds its own context on purpose. A likely cause of failure is the
// operation deadline expiring, and running the undo on an already-dead context
// would strand the container under its backup name — exactly the outcome this
// whole design exists to prevent.
//
// Errors are logged rather than returned: the caller is already reporting why the
// recreate failed, and there is nothing further to try.
func (m *Manager) rollback(ctx context.Context, oldID string, plan recreatePlan, progress recreateProgress, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	log.Warn("recreate failed, rolling back", "renamed", progress.renamed, "created", progress.newID != "")

	// The replacement must go first: while it exists it holds the original name.
	//
	// v=false again. A buggy plan could in principle have left real data on a
	// volume attached here, and an orphaned empty volume is a much smaller
	// problem than a deleted one.
	if progress.newID != "" {
		if err := m.docker.RemoveContainer(ctx, progress.newID, false); err != nil {
			log.Error("rollback could not remove the half-built replacement",
				"new_id", shortID(progress.newID), "error", err)
		}
	}

	if progress.renamed {
		if err := m.docker.RenameContainer(ctx, oldID, plan.Name); err != nil {
			log.Error("rollback could not restore the original name — "+
				"the container still exists under its backup name",
				"backup_name", plan.Name+backupSuffix, "error", err)
			return
		}
	}

	if progress.stopped {
		if err := m.docker.StartContainer(ctx, oldID); err != nil {
			log.Error("rollback could not restart the original container", "error", err)
			return
		}
	}
	log.Info("rollback complete, the original container is back as it was")
}
