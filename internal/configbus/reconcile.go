package configbus

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
)

// GenerationSource is deliberately smaller than Store so a node can reconcile
// against an API, a read replica, or a test double.
type GenerationSource interface {
	ListGenerations(context.Context) ([]Generation, error)
}

// ReloadFunc fetches the tenant snapshot from the control plane/config store.
// The generation is a fencing token: the callback must apply it only after it
// has loaded a snapshot at least as new as that value.
type ReloadFunc func(context.Context, tenant.ID, uint64) error

// Reconciler provides both pub/sub-triggered and periodic convergence. It
// updates its local generation only after Reload succeeds, so a failed reload
// is retried by the next notification or reconciliation pass.
type Reconciler struct {
	source GenerationSource
	reload ReloadFunc

	mu      sync.Mutex
	current map[tenant.ID]uint64
}

func NewReconciler(source GenerationSource, reload ReloadFunc) (*Reconciler, error) {
	if source == nil {
		return nil, errors.New("configbus: generation source is required")
	}
	if reload == nil {
		return nil, errors.New("configbus: reload callback is required")
	}
	return &Reconciler{source: source, reload: reload, current: make(map[tenant.ID]uint64)}, nil
}

// Observe applies a notification if it advances the local fence. Stale and
// duplicate notifications are cheap no-ops. The mutex is held through reload
// to serialize two concurrent notifications for the same node and prevent an
// older callback from completing after a newer one.
func (r *Reconciler) Observe(ctx context.Context, id tenant.ID, generation uint64) error {
	if id == "" || generation == 0 {
		return fmt.Errorf("configbus: invalid notification (%q, %d)", id, generation)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation <= r.current[id] {
		return nil
	}
	if err := r.reload(ctx, id, generation); err != nil {
		return err
	}
	r.current[id] = generation
	return nil
}

func (r *Reconciler) Generation(id tenant.ID) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current[id]
}

// Reconcile reads only generations, then invokes Observe for entries ahead of
// the local fence. A single bad tenant does not prevent other tenants from
// converging; all errors are joined for observability.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	rows, err := r.source.ListGenerations(ctx)
	if err != nil {
		return err
	}
	var joined error
	for _, row := range rows {
		if err := r.Observe(ctx, row.TenantID, row.Generation); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// Run periodically reconciles with a randomized initial delay. The jitter
// avoids every node querying the control plane at the same instant after a
// rolling deployment. Notification consumers can call Observe independently.
// A transient reconciliation error is reported to the optional callback and
// does not stop the loop; the next tick retries from the durable source.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration, onError ...func(error)) error {
	if interval <= 0 {
		return errors.New("configbus: reconciliation interval must be positive")
	}
	initial := time.Duration(rand.Int63n(int64(interval)))
	timer := time.NewTimer(initial)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	if err := r.Reconcile(ctx); err != nil {
		reportReconcileError(err, onError)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.Reconcile(ctx); err != nil {
				reportReconcileError(err, onError)
			}
		}
	}
}

func reportReconcileError(err error, callbacks []func(error)) {
	if len(callbacks) > 0 && callbacks[0] != nil {
		callbacks[0](err)
	}
}
