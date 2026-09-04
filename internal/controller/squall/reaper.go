// SPDX-License-Identifier: MIT

package squall

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
	"github.com/ackstorm/squall/internal/dstack"
)

// DefaultReapInterval is how often the reaper re-answers "is anything
// running that nothing owns?". One minute: a leaked GPU is billed by the
// second, and listing runs is one cheap call.
const DefaultReapInterval = time.Minute

// DefaultReapTimeout bounds one orphan sweep so a hung dstack or API-server
// call cannot leave the single reaper goroutine dark until restart.
const DefaultReapTimeout = 30 * time.Second

// DefaultReapGrace is how long a run must exist before the reaper will
// consider it an orphan. It exists to lose exactly one race: a run that
// Apply has created but whose Model the reaper has not observed yet. Five
// minutes is far longer than that window and far shorter than a bill worth
// worrying about.
const DefaultReapGrace = 5 * time.Minute

// Reaper is the answer to "we cannot afford a delete that silently does not
// happen". The finalizer is the primary teardown path; this is the audit
// that assumes the primary path failed.
//
// It is deliberately NOT a retry of the finalizer. It re-derives the truth
// from scratch every pass — every run dstack reports, against every Model
// the API server holds — so it recovers from causes the finalizer cannot:
// a controller killed mid-teardown, a CR force-deleted with its finalizer
// stripped, a run created by a Model that was deleted while the controller
// was down, an operator's `kubectl patch` removing the finalizer to unwedge
// something.
//
// Safety, in the order it matters:
//
//   - It NEVER acts on a partial view. If listing Models fails, the pass is
//     abandoned: an API server blip must not be read as "every Model is
//     gone, delete everything". This is the single most important line in
//     the file.
//   - It only touches runs OLDER than Grace, so it cannot race a wake.
//   - It stops before deleting (D56), and treats ErrNotFound as success.
//   - One bad run does not stop the sweep; each is reported and the pass
//     continues, because the whole point is that the next minute finds
//     less, not that the first failure hides the rest.
type Reaper struct {
	// Client MUST be an uncached reader (mgr.GetAPIReader()), never the
	// manager's informer-backed client. Sweep's own comment demands the
	// Model list come from the API server so a stale view cannot orphan a
	// live run — but a degraded reflector returns a SHORT list with a NIL
	// error, so the "refuse to reap on a partial view" abort below is
	// unreachable through a cache (D107). The type is client.Reader so a
	// cached client cannot even be wired here by accident: Reaper needs
	// nothing but List.
	Client       client.Reader
	DstackClient dstack.Client
	Interval     time.Duration
	Grace        time.Duration
	Timeout      time.Duration
	Clock        func() time.Time
}

// NeedLeaderElection keeps exactly one replica reaping. Two reapers racing
// would each see the other's in-flight stop as an un-reaped orphan.
func (r *Reaper) NeedLeaderElection() bool { return true }

// Start runs the sweep until the manager's context is cancelled. It is
// registered with mgr.Add in cmd/controller.
func (r *Reaper) Start(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = DefaultReapInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger := log.FromContext(ctx).WithName("reaper")
	logger.Info("orphan reaper started", "interval", interval, "grace", r.grace())

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if reaped, err := r.Sweep(ctx); err != nil {
				// Never fatal: the next tick re-derives everything.
				logger.Error(err, "orphan sweep failed; will retry", "reaped", reaped)
			}
		}
	}
}

func (r *Reaper) grace() time.Duration {
	if r.Grace <= 0 {
		return DefaultReapGrace
	}
	return r.Grace
}

func (r *Reaper) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// Sweep performs one pass and reports how many orphans it acted on. It is
// exported so a test can drive it deterministically rather than waiting on
// a ticker.
func (r *Reaper) Sweep(ctx context.Context) (int, error) {
	logger := log.FromContext(ctx).WithName("reaper")
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultReapTimeout
	}
	sweepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Owners FIRST, and from the API server rather than a cache: a stale
	// cache that has not yet seen a new Model would make its run look
	// orphaned. If this fails we do nothing at all — see the type comment.
	var models squallv1alpha1.ModelList
	if err := r.Client.List(sweepCtx, &models); err != nil {
		return 0, fmt.Errorf("list Models: refusing to reap on a partial view: %w", err)
	}
	owned := make(map[string]types.NamespacedName, 2*len(models.Items))
	liveUIDs := make(map[string]types.NamespacedName, len(models.Items))
	for i := range models.Items {
		m := &models.Items[i]
		nn := types.NamespacedName{Namespace: m.Namespace, Name: m.Name}
		// F1: a Model owns EVERY name its run could be filed under — the
		// namespace-qualified one and whatever status recorded (D109: an
		// earlier version of this comment also claimed the pre-F1 bare
		// name; that adoption path was removed once every legacy run was
		// terminal). Deliberately generous, because this set gates a
		// destructive action: a Model reconciled before it could persist
		// status.runName still owns its qualified run, and a sweep landing
		// in that window must not stop a live, paid replica.
		for _, name := range runNamesFor(m) {
			owned[name] = nn
		}
		liveUIDs[string(m.UID)] = nn
	}

	runs, err := r.DstackClient.ListRuns(sweepCtx)
	if err != nil {
		return 0, fmt.Errorf("list dstack runs: %w", err)
	}

	now := r.now()
	reaped := 0
	var errs []error
	for i := range runs {
		run := &runs[i]
		if _, isOwned := owned[run.Name]; isOwned {
			continue
		}
		if run.IsTerminal() {
			// Already dead. Nothing is billing, and dstack keeps terminal
			// runs listed by design (D52) — deleting them is tidiness, not
			// safety, so it is not worth an API call or a failure mode.
			continue
		}
		if age := now.Sub(run.SubmittedAt); age < r.grace() {
			logger.V(1).Info("unowned run is younger than the grace period; leaving it",
				"run", run.Name, "age", age)
			continue
		}

		// D108: a name miss alone is not evidence of orphanhood. ListRuns
		// answers from dstack's un-scoped root route, so on a shared server
		// the candidate set contains runs squall never minted — a
		// colleague's `dstack apply`, another project, a second squall
		// install — and stopping one of those on the strength of a string
		// miss destroys somebody else's live GPU job. Only a run carrying
		// squall's own SQUALL_MODEL_UID stamp (ownership.go) whose UID
		// matches no live Model is PROVABLY an orphan of ours. An unmarked
		// run is left alone and logged: it might be squall's from before
		// the stamp existed, in which case it bills until a human looks —
		// the tolerable error — or it might be someone else's, in which
		// case touching it is the intolerable one.
		markedUID := run.Env[ModelUIDEnvKey]
		if markedUID == "" {
			logger.Info("REFUSING to reap an unmarked run: no Model claims its name, but without a "+
				ModelUIDEnvKey+" stamp squall cannot prove it minted this run",
				"run", run.Name, "runID", run.RunID, "age", now.Sub(run.SubmittedAt))
			continue
		}
		if nn, alive := liveUIDs[markedUID]; alive {
			logger.Info("leaving a run stamped by a live Model despite a name mismatch; "+
				"reporting, not reaping — see D108/D109",
				"run", run.Name, "model", nn.String())
			continue
		}

		logger.Info("REAPING an orphaned dstack run: squall's own stamp names a Model that no longer exists",
			"run", run.Name, "runID", run.RunID, "modelUID", markedUID, "age", now.Sub(run.SubmittedAt))
		if err := r.DstackClient.Stop(sweepCtx, run.Name); err != nil && !errors.Is(err, dstack.ErrNotFound) {
			errs = append(errs, fmt.Errorf("stop orphan run %q: %w", run.Name, err))
			continue
		}
		reaped++
	}

	if len(errs) > 0 {
		return reaped, errors.Join(errs...)
	}
	return reaped, nil
}

var _ manager.Runnable = &Reaper{}
