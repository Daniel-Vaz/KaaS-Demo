package reconcile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/domain"
)

// This file is the reconciler's half of the cluster action history (the portal's "Operations
// history" on the Activity tab). The APP records the user-initiated operations - create, scale,
// add-ons, upgrade - when it writes desired state, and the generation sweep closes them once the
// cluster converges (see app.recordOp / store.CompleteOperations). The reconciler records the
// PLATFORM-initiated ones - the automated backups, certificate rotations, defragmentations and
// repairs it performs on its own - so the same history that shows "alice scaled workers 2→3" also
// shows "scheduled control-plane backup" and "auto-repair: restart kubelet on dvaz-default-0".
//
// These ops differ from the user-initiated ones in three ways, all of which follow from the fact
// that the platform, not a user, is the actor: they carry no actor, no generation, and they are
// opened and closed within the SAME phase invocation that does the work - so the id never has to
// survive across ticks, and the generation sweep skips them (domain.OperationKind.SweepExempt).
// A recording that spans a single invocation still shows as in_progress to a concurrent viewer,
// because RecordOperation commits immediately and a long backup/rebuild runs between the two calls.

// recordAutoOp appends a platform-initiated operation (in_progress, no actor, no generation) and
// returns its id - the handle completeAutoOp closes it with. Best-effort: a recording failure is
// logged and yields "", which completeAutoOp treats as a no-op, so the action history is never a
// reason a backup or repair fails.
func (r *Reconciler) recordAutoOp(clusterID string, kind domain.OperationKind, summary, detail string) string {
	op := &domain.Operation{
		ID:        newOpID(),
		ClusterID: clusterID,
		Kind:      kind,
		Summary:   summary,
		Detail:    detail,
		Status:    domain.OpInProgress,
		StartedAt: time.Now(),
	}
	if err := r.Store.RecordOperation(op); err != nil {
		r.Log.Warn("record operation", "cluster", clusterID, "kind", kind, "err", err)
		return ""
	}
	return op.ID
}

// completeAutoOp closes an operation recordAutoOp opened, stamping the outcome detail (empty leaves
// the recorded detail untouched - see store.CompleteOperation). A "" id (a failed record) is a
// no-op, so callers can wrap every action in record/complete without branching on the record result.
func (r *Reconciler) completeAutoOp(id, detail string) {
	if id == "" {
		return
	}
	if err := r.Store.CompleteOperation(id, detail, time.Now()); err != nil {
		r.Log.Warn("complete operation", "id", id, "err", err)
	}
}

// repairOpSummary is the one-line action-history summary for an auto-repair rung - the operator's
// "what did the platform just do to my cluster" sentence, distinct from the finer-grained events the
// repair path also emits.
func repairOpSummary(action domain.RepairAction, vmName string) string {
	switch action {
	case domain.ActionPowerOn:
		return "Auto-repair: power on " + vmName
	case domain.ActionRejoin:
		return "Auto-repair: rejoin " + vmName
	case domain.ActionRestartKubelet:
		return "Auto-repair: restart kubelet on " + vmName
	case domain.ActionReplace:
		return "Auto-repair: rebuild node " + vmName
	case domain.ActionRestore:
		return "Auto-repair: restore " + vmName + " from backup"
	}
	return "Auto-repair: " + string(action) + " " + vmName
}

// repairOpDetail is the fault + attempt context an auto-repair op carries. Read from the target's
// per-node state, which RecordAttempt has already stamped by the time PhaseRepairing runs.
func (r *Reconciler) repairOpDetail(rs *domain.ClusterRepair) string {
	st := rs.State(rs.Target)
	return fmt.Sprintf("%s · attempt %d of %d", st.Fault, st.Attempts, r.RepairPolicy.MaxAttempts)
}

// newOpID mints an operation's identity - salted so two workers acting on different clusters in the
// same instant cannot collide on the primary key. Same shape as newSnapshotID.
func newOpID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("op-%d-%s", time.Now().UTC().UnixNano(), hex.EncodeToString(b))
}
