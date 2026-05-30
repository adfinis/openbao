// Copyright (c) 2025 OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/hashicorp/go-hclog"
	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/helper/fairshare"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/vault/barrier"
	"github.com/openbao/openbao/vault/policy"
	"github.com/openbao/openbao/vault/quotas"
)

const (
	dispatcherName          = "invalidate-dispatch"
	refresherName           = "invalidate-cache-refresh"
	maxLockTime             = 2 * time.Second
	maxInvalidateTime       = 30 * time.Second
	maxPluginInvalidateTime = 2 * time.Second
	maxDispatchers          = 128

	drKeyTransitionTimeout        = 90 * time.Second
	drKeyTransitionBackoffMin     = 100 * time.Millisecond
	drKeyTransitionBackoffMax     = 2 * time.Second
	drKeyTransitionDeferredKeyCap = 20000
)

var errDRKeyTransitionQueueOverflow = errors.New("DR key-transition deferred invalidation queue overflow")

func (c *Core) Invalidate(key ...string) {
	c.invalidations.Add(key...)
}

func (c *Core) invalidateSynchronous(key string) {
	job, _ := c.invalidations.buildInvalidateJobForKey(make(chan struct{}), context.Background(), key)
	if err := job.Execute(); err != nil {
		job.OnFailure(err)
	}
}

// invalidationManager is a long-lived subset of Core which is used to handle
// storage-level invalidations.
type invalidationManager struct {
	core *Core

	// Invalidate stages pending invalidations into this queue.
	pendingLock   sync.Mutex
	enabled       atomic.Bool
	pending       []string
	pendingNotify chan struct{}

	// quitCh notifies that we should stop actively processing invalidations.
	//
	// We'll still keep appending to pending, though, assuming enabled=true
	quitCh      chan struct{}
	quitContext context.Context
	doneCh      chan struct{}

	// dispatcher handles processing events from the invalidation queue to
	// subsystems. This is handled separately so that the storage layer
	// doesn't need to make asynchronous calls to the hook, while allowing
	// actual invalidation processing to take longer.
	dispacherLogger log.Logger
	dispatcher      *fairshare.JobManager
}

func (core *Core) NewInvalidationManager() {
	core.invalidations = &invalidationManager{
		core:            core,
		dispacherLogger: core.logger.Named(dispatcherName),

		pendingNotify: make(chan struct{}),
	}
}

func (im *invalidationManager) Track() {
	// Clear any leftover remaining items.
	im.pendingLock.Lock()
	im.pending = nil
	im.pendingLock.Unlock()

	// Start tracking new changes.
	im.enabled.Store(true)
}

func (im *invalidationManager) Start(ctx context.Context) {
	im.dispatcher = fairshare.NewJobManager(dispatcherName, maxDispatchers, im.dispacherLogger, im.core.metricSink)
	im.dispatcher.Start()

	im.quitCh = make(chan struct{})
	im.quitContext = ctx
	im.doneCh = make(chan struct{})

	// Now that we've started, start processing pending invalidations until
	// told to stop.
	go im.processPendingQueue(im.quitCh, im.quitContext, im.doneCh)
}

func (im *invalidationManager) Stop() error {
	im.dispacherLogger.Debug("stop triggered")
	defer im.dispacherLogger.Debug("finished stopping")

	// Prevent enqueuing more items.
	im.enabled.Store(false)

	// Close the quit channel to cancel any yet-to-be-dispatched.
	if im.quitCh != nil {
		close(im.quitCh)
	}

	// Stop processing the ones we have.
	if im.dispatcher != nil {
		im.dispatcher.Stop()
	}

	// Wait for the processing queue to finish.
	if im.doneCh != nil {
		timeout := time.NewTimer(maxInvalidateTime)
		select {
		case <-timeout.C:
			im.dispacherLogger.Warn("failed to stop processing queue")
		case <-im.doneCh:
		}

		if !timeout.Stop() {
			<-timeout.C
		}
	}

	// Clear any remaining items.
	im.pendingLock.Lock()
	im.pending = nil
	im.pendingLock.Unlock()

	// Clear any start-specific state.
	im.quitCh = nil
	im.quitContext = nil
	im.dispatcher = nil
	im.doneCh = nil

	return nil
}

func (im *invalidationManager) processPendingQueue(quitCh chan struct{}, quitContext context.Context, doneCh chan struct{}) {
	go func() {
		defer close(doneCh)
		for {
			select {
			case <-quitCh:
				im.dispacherLogger.Debug("shutting down; skipping pending queue processing")
				return
			case <-quitContext.Done():
				im.dispacherLogger.Debug("core context canceled, skipping pending queue processing")
				return
			case <-im.pendingNotify:
			}

			func() {
				defer metrics.MeasureSince([]string{dispatcherName, "enqueue-pending"}, time.Now())

				im.pendingLock.Lock()
				pending := im.pending
				im.pending = nil
				im.pendingLock.Unlock()

				im.core.metricSink.SetGauge([]string{dispatcherName, "pending-dequeue-size"}, float32(len(pending)))

				for _, key := range pending {
					job, queue := im.buildInvalidateJobForKey(quitCh, quitContext, key)
					im.dispatcher.AddJob(job, queue)
				}
			}()
		}
	}()
}

func (im *invalidationManager) buildInvalidateJobForKey(quitCh chan struct{}, quitContext context.Context, key string) (fairshare.Job, string) {
	// Fairshare ensures we don't starve other queues too long. We need to
	// balance some things here:
	//
	// 1. Total memory consumption.
	// 2. Not starving any one namespace based on the work of others.
	// 3. Prioritizing Core tasks over others.
	//
	// Thus if a change comes into the root namespace's core/, it'll be
	// dispatched on its own queue by key, but all other work in the root
	// namespace or child namespaces will be in their own queues (minus,
	// again, a namespace's core, which will now share a queue).
	//
	// This balances the total number of queues (in most _reasonable_ systems),
	// while allowing prioritization of core updates and prioritizing root
	// updates most of all.
	ns, subkey := im.splitNamespaceFromKey(key)

	queue := ns
	if ns == namespace.RootNamespaceUUID {
		if strings.HasPrefix(subkey, "core/") {
			queue = key
		}
	} else if strings.HasPrefix(subkey, "core/") {
		queue += "-core"
	}

	return &invalidationJob{
		quitCh:      quitCh,
		quitContext: quitContext,
		im:          im,
		key:         key,
		nsUUID:      ns,
		nsKey:       subkey,
	}, queue
}

type invalidationJob struct {
	quitCh      chan struct{}
	quitContext context.Context

	im     *invalidationManager
	key    string
	nsUUID string
	nsKey  string

	fatal bool
}

func isLegacyMountPath(key string) bool {
	return key == coreMountConfigPath ||
		key == coreLocalMountConfigPath ||
		key == coreAuthConfigPath ||
		key == coreLocalAuthConfigPath
}

func isTransactionalMountPath(key string) bool {
	return strings.HasPrefix(key, coreMountConfigPath+"/") ||
		strings.HasPrefix(key, coreLocalMountConfigPath+"/") ||
		strings.HasPrefix(key, coreAuthConfigPath+"/") ||
		strings.HasPrefix(key, coreLocalAuthConfigPath+"/")
}

func isKeyringPath(key string) bool {
	return key == barrierSealConfigPath ||
		key == barrier.KeyringPath ||
		key == barrier.LegacyRootKeyPath ||
		key == recoverySealConfigPath ||
		key == recoveryKeyPath ||
		key == barrier.RootKeyPath ||
		key == barrier.ShamirKekPath ||
		key == StoredBarrierKeysPath ||
		strings.HasPrefix(key, barrier.KeyringUpgradePrefix)
}

func isMissedMountKey(key string) bool {
	return strings.HasPrefix(key, barrier.CredentialBarrierPrefix) ||
		strings.HasPrefix(key, backendBarrierPrefix) ||
		strings.HasPrefix(key, auditBarrierPrefix)
}

func isLoginMFA(key string) bool {
	return strings.HasPrefix(key, barrier.SystemBarrierPrefix+loginMFAConfigPrefix) ||
		strings.HasPrefix(key, barrier.SystemBarrierPrefix+mfaLoginEnforcementPrefix)
}

type drSecondaryKeyTransitionState struct {
	mu sync.Mutex

	active     bool
	replaying  bool
	deadline   time.Time
	generation uint64

	deferredKeys []string
	deferredSet  map[string]struct{}

	workerRunning    bool
	workerGeneration uint64
}

func errorChainContains(err error, matcher func(error) bool) bool {
	if err == nil || matcher == nil {
		return false
	}

	stack := []error{err}
	for depth := 0; len(stack) > 0 && depth < 256; depth++ {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == nil {
			continue
		}

		if matcher(cur) {
			return true
		}

		switch wrapped := any(cur).(type) {
		case interface{ Unwrap() error }:
			stack = append(stack, wrapped.Unwrap())
		case interface{ Unwrap() []error }:
			stack = append(stack, wrapped.Unwrap()...)
		}
	}

	return false
}

func isTransientBarrierDecryptFailure(err error) bool {
	if errors.Is(err, barrier.ErrBarrierInvalidKey) {
		return true
	}
	return errorChainContains(err, func(cur error) bool {
		msg := strings.ToLower(cur.Error())
		return strings.Contains(msg, "cipher: message authentication failed") ||
			(strings.Contains(msg, "decryption failed") && strings.Contains(msg, "authentication failed")) ||
			(strings.Contains(msg, "failed to decrypt") && strings.Contains(msg, "authentication failed"))
	})
}

func isReadOnlyStorageError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, logical.ErrReadOnly) {
		return true
	}

	return errorChainContains(err, func(cur error) bool {
		msg := strings.ToLower(cur.Error())
		return strings.Contains(msg, "readonly storage") ||
			strings.Contains(msg, "read-only storage")
	})
}

func isTransitionTransientInvalidationError(err error, transitionActive bool) bool {
	if err == nil {
		return false
	}
	if isTransientBarrierDecryptFailure(err) {
		return true
	}
	if transitionActive && isReadOnlyStorageError(err) {
		return true
	}
	return transitionActive && errors.Is(err, errLoadAuditFailed)
}

func (c *Core) beginDRKeyTransition(reason string) (uint64, bool) {
	if c == nil {
		return 0, false
	}

	now := time.Now()
	state := &c.drSecondaryKeyTransition

	state.mu.Lock()
	started := false
	switch {
	case !state.active || now.After(state.deadline):
		state.active = true
		state.replaying = false
		state.generation++
		state.deadline = now.Add(drKeyTransitionTimeout)
		state.workerRunning = false
		state.workerGeneration = 0
		started = true
	case state.replaying:
		// Start a fresh generation when a new transition event arrives while
		// replaying deferred keys.
		state.generation++
		state.replaying = false
		state.deadline = now.Add(drKeyTransitionTimeout)
		state.workerRunning = false
		state.workerGeneration = 0
		started = true
	}
	if state.deferredSet == nil {
		state.deferredSet = make(map[string]struct{})
	}
	generation := state.generation
	deadline := state.deadline
	deferredCount := len(state.deferredKeys)
	state.mu.Unlock()

	if started {
		metrics.IncrCounter([]string{"replication", "dr", "secondary", "key_transition_started_total"}, 1)
		c.logger.Warn("DR key transition started", "generation", generation, "deadline", deadline, "reason", reason)
	}
	metrics.SetGauge([]string{"replication", "dr", "secondary", "key_transition_deferred_keys_current"}, float32(deferredCount))

	return generation, started
}

func (c *Core) isDRKeyTransitionActive() bool {
	if c == nil {
		return false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	return state.active
}

func (c *Core) isDRKeyTransitionReplaying() bool {
	if c == nil {
		return false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	return state.active && state.replaying
}

func (c *Core) isDRKeyTransitionGenerationActive(generation uint64) bool {
	if c == nil {
		return false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	return state.active && state.generation == generation
}

func (c *Core) drKeyTransitionDeadline(generation uint64) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.active || state.generation != generation {
		return time.Time{}, false
	}
	return state.deadline, true
}

func (c *Core) markDRKeyTransitionWorkerStarted(generation uint64) bool {
	if c == nil {
		return false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.active || state.generation != generation {
		return false
	}
	if state.workerRunning && state.workerGeneration == generation {
		return false
	}

	state.workerRunning = true
	state.workerGeneration = generation
	return true
}

func (c *Core) markDRKeyTransitionWorkerStopped(generation uint64) {
	if c == nil {
		return
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.workerGeneration == generation {
		state.workerRunning = false
	}
}

func (c *Core) markDRKeyTransitionReplaying(generation uint64, replaying bool) bool {
	if c == nil {
		return false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.active || state.generation != generation {
		return false
	}
	state.replaying = replaying
	return true
}

func (c *Core) recordDeferredInvalidation(key string) (uint64, error) {
	if c == nil {
		return 0, fmt.Errorf("core unavailable for deferred invalidation")
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.active {
		return 0, fmt.Errorf("DR key transition is not active")
	}
	if state.deferredSet == nil {
		state.deferredSet = make(map[string]struct{})
	}
	if _, exists := state.deferredSet[key]; exists {
		return state.generation, nil
	}
	if len(state.deferredKeys) >= drKeyTransitionDeferredKeyCap {
		return state.generation, errDRKeyTransitionQueueOverflow
	}

	state.deferredSet[key] = struct{}{}
	state.deferredKeys = append(state.deferredKeys, key)
	metrics.SetGauge([]string{"replication", "dr", "secondary", "key_transition_deferred_keys_current"}, float32(len(state.deferredKeys)))
	return state.generation, nil
}

func (c *Core) snapshotDeferredInvalidations(generation uint64) ([]string, bool) {
	if c == nil {
		return nil, false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	defer state.mu.Unlock()

	if !state.active || state.generation != generation {
		return nil, false
	}

	keys := make([]string, len(state.deferredKeys))
	copy(keys, state.deferredKeys)
	return keys, true
}

func (c *Core) endDRKeyTransition(generation uint64) bool {
	if c == nil {
		return false
	}

	state := &c.drSecondaryKeyTransition
	state.mu.Lock()
	if !state.active || state.generation != generation {
		state.mu.Unlock()
		return false
	}

	deferredCount := len(state.deferredKeys)
	state.active = false
	state.replaying = false
	state.deadline = time.Time{}
	state.deferredKeys = nil
	state.deferredSet = nil
	state.workerRunning = false
	state.workerGeneration = 0
	state.mu.Unlock()

	metrics.SetGauge([]string{"replication", "dr", "secondary", "key_transition_deferred_keys_current"}, 0)
	c.logger.Info("DR key transition completed", "generation", generation, "deferred_keys", deferredCount)
	return true
}

func (ij *invalidationJob) isDRSecondaryNode() bool {
	core := ij.im.core
	if core == nil {
		return false
	}
	if core.drManager != nil && core.drManager.Mode() == DRModeSecondary {
		return true
	}
	return core.isDRKeyTransitionActive()
}

func (ij *invalidationJob) deferInvalidation(reason string) error {
	core := ij.im.core
	if core == nil {
		return nil
	}

	generation, _ := core.beginDRKeyTransition(reason)
	generation, err := core.recordDeferredInvalidation(ij.key)
	if err != nil {
		if errors.Is(err, errDRKeyTransitionQueueOverflow) {
			metrics.IncrCounter([]string{"replication", "dr", "secondary", "key_transition_queue_overflow_total"}, 1)
			ij.im.dispacherLogger.Error("DR key-transition deferred queue overflow; failing closed",
				"generation", generation, "key", ij.key, "cap", drKeyTransitionDeferredKeyCap)
		}
		ij.fatal = true
		return err
	}

	ij.im.ensureDRKeyTransitionWorker(generation)
	return nil
}

func (ij *invalidationJob) isDecryptSensitiveInvalidationKey(key string) bool {
	switch {
	case strings.HasPrefix(key, namespaceStoreSubPath):
		return true
	case strings.HasPrefix(key, barrier.SystemBarrierPrefix+quotas.StoragePrefix):
		return true
	case key == coreAuditConfigPath || key == coreLocalAuditConfigPath:
		return true
	case isLegacyMountPath(key):
		return true
	case isTransactionalMountPath(key):
		return true
	case isMissedMountKey(ij.key):
		return true
	default:
		return false
	}
}

func (ij *invalidationJob) shouldDeferInvalidation(key string) bool {
	if !ij.isDRSecondaryNode() {
		return false
	}

	core := ij.im.core
	if core == nil || !core.isDRKeyTransitionActive() || core.isDRKeyTransitionReplaying() {
		return false
	}

	if key == drConfigPath || isKeyringPath(key) {
		return false
	}

	return ij.isDecryptSensitiveInvalidationKey(key)
}

func (ij *invalidationJob) drConfigInvalidation(ctx context.Context) error {
	core := ij.im.core
	if core == nil {
		return nil
	}

	generation, _ := core.beginDRKeyTransition("dr config invalidation")
	ij.im.ensureDRKeyTransitionWorker(generation)

	if core.drManager == nil {
		ij.fatal = true
		return fmt.Errorf("DR config invalidation received but DR manager is unavailable")
	}

	mode, err := core.drManager.RefreshConfigFromStorage(ctx)
	if err != nil {
		if isTransitionTransientInvalidationError(err, true) {
			ij.im.dispacherLogger.Warn("transient failure refreshing DR config during key transition",
				"generation", generation, "key", ij.key, "error", err)
			return nil
		}
		ij.fatal = true
		return fmt.Errorf("failed to refresh DR config from invalidation: %w", err)
	}

	if mode != DRModeSecondary {
		core.endDRKeyTransition(generation)
	}

	return nil
}

func (ij *invalidationJob) keyringInvalidation() error {
	core := ij.im.core
	if core == nil {
		return nil
	}

	generation, _ := core.beginDRKeyTransition("keyring invalidation")
	ij.im.ensureDRKeyTransitionWorker(generation)
	return nil
}

func (ij *invalidationJob) executePotentiallyFatalInvalidation(ctx context.Context, operation string, fn func(context.Context) error) error {
	err := fn(ctx)
	if err == nil {
		return nil
	}

	transitionActive := false
	if ij.im != nil && ij.im.core != nil {
		transitionActive = ij.im.core.isDRKeyTransitionActive()
	}

	if ij.isDRSecondaryNode() && isTransitionTransientInvalidationError(err, transitionActive) {
		ij.im.dispacherLogger.Warn("deferring decrypt-sensitive invalidation during DR key transition",
			"operation", operation, "key", ij.key, "error", err, "transition_active", transitionActive)
		return ij.deferInvalidation(fmt.Sprintf("transient decrypt failure in %s", operation))
	}

	ij.fatal = true
	return err
}

func (im *invalidationManager) ensureDRKeyTransitionWorker(generation uint64) {
	if im == nil || im.core == nil || generation == 0 {
		return
	}

	if !im.core.markDRKeyTransitionWorkerStarted(generation) {
		return
	}

	go im.runDRKeyTransitionWorker(generation)
}

func (im *invalidationManager) runDRKeyTransitionWorker(generation uint64) {
	defer im.core.markDRKeyTransitionWorkerStopped(generation)

	backoff := drKeyTransitionBackoffMin
	for {
		deadline, ok := im.core.drKeyTransitionDeadline(generation)
		if !ok {
			return
		}

		if time.Now().After(deadline) {
			metrics.IncrCounter([]string{"replication", "dr", "secondary", "key_transition_timeout_total"}, 1)
			im.dispacherLogger.Error("DR key transition timed out; failing closed",
				"generation", generation, "deadline", deadline)
			im.core.restart()
			return
		}

		metrics.IncrCounter([]string{"replication", "dr", "secondary", "key_transition_resync_attempts_total"}, 1)
		err := im.drKeyTransitionResyncAttempt(generation)
		if err == nil {
			metrics.IncrCounter([]string{"replication", "dr", "secondary", "key_transition_resync_success_total"}, 1)
			im.replayDeferredInvalidations(generation)
			return
		}

		im.dispacherLogger.Warn("DR key-transition resync attempt failed",
			"generation", generation, "error", err)
		if !isTransitionTransientInvalidationError(err, true) {
			im.dispacherLogger.Error("fatal DR key-transition resync error; failing closed",
				"generation", generation, "error", err)
			im.core.restart()
			return
		}

		if !im.drKeyTransitionWait(backoff) {
			return
		}
		backoff *= 2
		if backoff > drKeyTransitionBackoffMax {
			backoff = drKeyTransitionBackoffMax
		}
	}
}

func (im *invalidationManager) drKeyTransitionWait(wait time.Duration) bool {
	if wait <= 0 {
		return true
	}

	jittered := wait
	if wait > time.Millisecond {
		lower := wait / 2
		jittered = lower + time.Duration(rand.Int63n(int64(wait-lower)+1))
	}

	timer := time.NewTimer(jittered)
	defer timer.Stop()

	var quitContextDone <-chan struct{}
	if im.quitContext != nil {
		quitContextDone = im.quitContext.Done()
	}

	select {
	case <-timer.C:
		return true
	case <-im.quitCh:
		return false
	case <-quitContextDone:
		return false
	}
}

func (im *invalidationManager) drKeyTransitionResyncAttempt(generation uint64) error {
	core := im.core
	if core == nil || core.barrier == nil {
		return fmt.Errorf("barrier unavailable for DR key-transition resync")
	}

	ctx, cancel := context.WithTimeout(context.Background(), maxInvalidateTime)
	defer cancel()

	if err := core.barrier.ReloadRootKey(ctx); err != nil {
		if isTransientBarrierDecryptFailure(err) {
			if recoverErr := im.recoverDRRootKeyFromStoredKeys(ctx); recoverErr != nil {
				return fmt.Errorf("failed to reload root key: %w (stored-key recovery failed: %v)", err, recoverErr)
			}
			im.dispacherLogger.Info("recovered DR root key from stored keys after decrypt mismatch", "generation", generation)
		} else {
			return fmt.Errorf("failed to reload root key: %w", err)
		}
	}
	if err := core.barrier.ReloadKeyring(ctx); err != nil {
		if isTransientBarrierDecryptFailure(err) {
			if recoverErr := im.recoverDRRootKeyFromStoredKeys(ctx); recoverErr != nil {
				return fmt.Errorf("failed to reload keyring: %w (stored-key recovery failed: %v)", err, recoverErr)
			}
			im.dispacherLogger.Info("recovered DR keyring from stored keys after decrypt mismatch", "generation", generation)
		} else {
			return fmt.Errorf("failed to reload keyring: %w", err)
		}
	}
	if err := core.ensureRaftTLSKeyringForDRSecondary(ctx); err != nil {
		return fmt.Errorf("failed to ensure raft TLS keyring for DR secondary: %w", err)
	}
	if err := core.checkRaftTLSKeyUpgrades(ctx); err != nil {
		return fmt.Errorf("failed to refresh raft TLS keyring: %w", err)
	}
	if core.drManager != nil {
		if _, err := core.drManager.RefreshConfigFromStorage(ctx); err != nil {
			return fmt.Errorf("failed to refresh DR config from storage: %w", err)
		}
	}

	im.dispacherLogger.Info("DR key-transition resync attempt succeeded", "generation", generation)
	return nil
}

func (im *invalidationManager) recoverDRRootKeyFromStoredKeys(ctx context.Context) error {
	core := im.core
	if core == nil || core.seal == nil || core.barrier == nil {
		return fmt.Errorf("core, seal, or barrier unavailable for stored-key recovery")
	}

	keys, err := core.seal.GetStoredKeys(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch stored keys: %w", err)
	}
	if len(keys) == 0 || len(keys[0]) == 0 {
		return fmt.Errorf("stored keys are empty")
	}

	if err := core.barrier.SetRootKey(keys[0]); err != nil {
		return fmt.Errorf("failed to set barrier root key from stored key: %w", err)
	}
	if err := core.barrier.ReloadKeyring(ctx); err != nil {
		return fmt.Errorf("failed to reload keyring after stored-key recovery: %w", err)
	}

	return nil
}

func (im *invalidationManager) replayDeferredInvalidations(generation uint64) {
	core := im.core
	if core == nil {
		return
	}

	keys, ok := core.snapshotDeferredInvalidations(generation)
	if !ok {
		return
	}

	if !core.markDRKeyTransitionReplaying(generation, true) {
		return
	}
	defer core.markDRKeyTransitionReplaying(generation, false)

	im.dispacherLogger.Info("starting DR key-transition replay",
		"generation", generation, "deferred_keys", len(keys))

	for _, key := range keys {
		if !core.isDRKeyTransitionGenerationActive(generation) {
			im.dispacherLogger.Info("stopping DR key-transition replay due to generation handoff",
				"generation", generation)
			return
		}
		im.Add(key)
	}

	metrics.IncrCounter([]string{"replication", "dr", "secondary", "key_transition_replay_success_total"}, float32(len(keys)))
	im.dispacherLogger.Info("completed DR key-transition replay",
		"generation", generation, "replayed_keys", len(keys))
	core.endDRKeyTransition(generation)
}

func (ij *invalidationJob) Execute() error {
	ij.im.dispacherLogger.Trace("processing invalidation", "key", ij.key)
	defer ij.im.dispacherLogger.Trace("concluding processing of invalidation", "key", ij.key)

	defer metrics.MeasureSince([]string{dispatcherName, "execute-invalidate"}, time.Now())
	ij.im.core.metricSink.IncrCounterWithLabels([]string{dispatcherName, "pending-dequeue-size"}, 1.0, nil)

	// Exit early if we're shut down before we get a chance to execute.
	select {
	case <-ij.quitCh:
		ij.im.dispacherLogger.Debug("shutting down; skipping job", "key", ij.key)
		return nil
	case <-ij.quitContext.Done():
		ij.im.dispacherLogger.Debug("core context canceled, skipping job", "key", ij.key)
		return nil
	default:
	}

	if ij.im.core.Sealed() {
		ij.im.dispacherLogger.Trace("refusing to process event when core is sealed", "key", ij.key)
		return nil
	}

	// State lock acquisition is usually very fast: we have many potential
	// readers of it and it only gets exclusively locked when HA status
	// changes, in which case, we're likely to restart our own state anyways.
	lockCtx, lockCancel := context.WithTimeout(ij.quitContext, maxLockTime)
	defer lockCancel()

	// Acquire state lock. We're running in a goroutine; while active context
	// should be sufficient to prevent races here, we want to ensure no core
	// state changes during processing of the request. We bind this to our
	// time-limited channel to ensure it processes in a reasonable amount of
	// time.
	l := newLockGrabber(ij.im.core.stateLock.RLock, ij.im.core.stateLock.RUnlock, lockCtx.Done())
	go l.grab()

	if stopped := l.lockOrStop(); stopped {
		// Failure to grab a lock is never fatal: HA state change will mean
		// that we'll restart the invalidation manager anyways.
		ij.im.dispacherLogger.Trace("unable to acquire read statelock", "key", ij.key, "context", ij.quitContext.Err())
		return nil
	}

	defer ij.im.core.stateLock.RUnlock()

	// Any storage operations we dispatch here should be time-bounded and
	// context refreshed.
	ctx, cancel := context.WithTimeout(ij.quitContext, maxInvalidateTime)
	defer cancel()

	// Always refresh physical cache for operations performed during
	// invalidation.
	ctx = physical.CacheRefreshContext(ctx, true)

	// Notify physical cache that our entry is stale if it is cached. This
	// ensures parallel reads see up-to-date data now that we're processing
	// invalidations.
	ij.im.core.physicalCache.Invalidate(ctx, ij.key)

	// Get a full namespace entry; it may be out of date since when the job
	// started and we need it for routing.
	//
	// Note that we never need to invalidate the namespace store here, before
	// we fetch this: namespace invalidation happens when the entry for the
	// child namespace is updated in the parent namespace, invalidating the
	// child. But the namespace UUID we have here is of the parent, which
	// (while it might be stale), cannot yet be invalidated as a separate
	// invalidation would occur for that. The exception of course is the root
	// namespace which is a virtual, storage-less namespace.
	ns, err := ij.im.core.namespaceStore.GetNamespace(ctx, ij.nsUUID)
	if err != nil {
		ij.fatal = true
		return fmt.Errorf("failed to load namespace %q from store: %w", ij.nsUUID, err)
	}
	if ns == nil {
		// Namespace was deleted; this is safe to ignore, because it occurs in
		// one of two scenarios:
		//
		// 1. The namespace was deleted already (invalidation on
		//    core/namespaces/<uuid> was processed first) and we're getting an
		//    invalidation for a child entry.
		// 2. The namespace has just been created (and the
		//    core/namespaces/<uuid> key was not yet invalidated) and we're
		//    getting an invalidation for the child entry.
		//
		// We will also receive a subsequent invalidation request for the
		// core/namespaces/<uuid> key in both cases, so we are fine to exit
		// silently.
		return nil
	}

	ctx = namespace.ContextWithNamespace(ctx, ns)

	// Lastly, create a short version of the context for plugin invalidations.
	shortCtx, shortCancel := context.WithTimeout(ctx, maxPluginInvalidateTime)
	defer shortCancel()

	// Now handle the actual event.
	key := ij.nsKey
	if ij.shouldDeferInvalidation(key) {
		return ij.deferInvalidation(fmt.Sprintf("transition active for %s", key))
	}

	switch {
	case strings.HasPrefix(key, namespaceStoreSubPath):
		return ij.executePotentiallyFatalInvalidation(ctx, "namespace", ij.namespaceInvalidation)
	case strings.HasPrefix(key, barrier.SystemBarrierPrefix+policy.ACLSubPath):
		// Policy invalidation is not fatal as it contains a LRU cache: we
		// know removal is strict and it is only potentially preloading an
		// entry which may err.
		return ij.policyInvalidation(ctx)
	case strings.HasPrefix(key, barrier.SystemBarrierPrefix+quotas.StoragePrefix):
		return ij.executePotentiallyFatalInvalidation(ctx, "quota", ij.quotaInvalidation)
	case key == coreAuditConfigPath || key == coreLocalAuditConfigPath:
		return ij.executePotentiallyFatalInvalidation(ctx, "audit", ij.auditInvalidation)
	case isLegacyMountPath(key):
		return ij.executePotentiallyFatalInvalidation(ctx, "legacy_mount", ij.legacyMountInvalidation)
	case isTransactionalMountPath(key):
		return ij.executePotentiallyFatalInvalidation(ctx, "transactional_mount", ij.transactionalMountInvalidation)
	case key == drConfigPath:
		return ij.drConfigInvalidation(ctx)
	case isKeyringPath(key):
		return ij.keyringInvalidation()
	case strings.HasPrefix(ij.key, coreLeaderPrefix):
		// The HA subsystem handles leadership changes.
	case strings.HasPrefix(ij.key, pluginCatalogPath):
		// There is nothing to do to invalidate a plugin catalog write.
	case ij.key == CoreLockPath:
		// The lock path isn't really a key that we invalidate; it is a lock
		// file written by some backends which lack an out-of-storage locking
		// mechanism. It is also handled by the HA mechanism and so is safe
		// to ignore.
	case strings.HasPrefix(ij.key, "autopilot/") || ij.key == raftAutopilotConfigurationStoragePath:
		// Raft context is reloaded when a standby becomes active, so it is
		// safe to ignore changes to autopilot state.
	case isLoginMFA(ij.key):
		ij.fatal = true
		return ij.loginMFAInvalidation(ctx, ns)
	case ij.im.core.router.Invalidate(shortCtx, ij.key):
		// if router.Invalidate returns true, a matching plugin was found and
		// the invalidation is therefore dispatched.
	case isMissedMountKey(ij.key):
		// router.Invalidate(...) may return false when a matching plugin was
		// not yet loaded, even though this was under a path we'd expect to
		// be a mount key (auth/, audit/, or logical/) prefix. Ignoring it is
		// fine: a later change to a subsequent entry will actually load the
		// mount, loading any data this entry would've contained for the first
		// time.
		//
		// This is true in reverse for deletions.
	default:
		ij.im.dispacherLogger.Warn("no mechanism to invalidate cache for specified key", "key", key)
	}

	return nil
}

func (ij *invalidationJob) namespaceInvalidation(ctx context.Context) error {
	ij.im.dispacherLogger.Trace("issuing namespace invalidation")

	// The namespace name is the final path segment; ij.nsUUID contains the
	// parent namespace UUID.
	namespaceUUID := strings.TrimPrefix(ij.nsKey, namespaceStoreSubPath)

	beforeNs, beforeErr := ij.im.core.namespaceStore.GetNamespace(ctx, namespaceUUID)

	// First notify the namespace storage that our next lookup might be stale.
	ij.im.core.namespaceStore.invalidate(ctx, ij.key)

	afterNs, afterErr := ij.im.core.namespaceStore.GetNamespace(ctx, namespaceUUID)

	// There are three happy paths for namespace invalidation:
	//
	// 1. Namespace deletion; before the namespace would be present but
	//    afterwards it would be nil.
	// 2. Namespace creation; before the namespace would be missing and
	//    afterwards it will be present.
	// 3. Namespace update; this exists in both places but we'd prefer the
	//    updated version.
	var preferredNs *namespace.Namespace
	var deleted bool
	switch {
	case beforeErr != nil && afterErr != nil:
		return fmt.Errorf("failed loading invalidated namespace; before=%w; after=%v", beforeErr, afterErr)
	case beforeNs == nil && afterNs == nil:
		return errors.New("failed loading invalidated namespace: does not exist before or after")
	case afterNs != nil && afterErr == nil:
		preferredNs = afterNs
	case beforeNs != nil && beforeErr == nil:
		preferredNs = beforeNs
		deleted = true
	}

	childCtx := namespace.ContextWithNamespace(ctx, preferredNs)

	// Invalidate all policies within the namespace.
	ij.im.core.policyStore.InvalidateNamespace(childCtx, namespaceUUID)

	// Now reload all mounts within the namespace.
	if err := ij.im.core.reloadNamespaceMounts(childCtx, namespaceUUID, deleted); err != nil {
		return fmt.Errorf("unable to invalidate mounts in namespace %q: %w", ij.nsUUID, err)
	}

	return nil
}

func (ij *invalidationJob) policyInvalidation(ctx context.Context) error {
	policyPath := strings.TrimPrefix(ij.nsKey, barrier.SystemBarrierPrefix+policy.ACLSubPath)
	return ij.im.core.policyStore.Invalidate(ctx, policyPath, policy.TypeACL)
}

func (ij *invalidationJob) quotaInvalidation(ctx context.Context) error {
	quotaPath := strings.TrimPrefix(ij.nsKey, barrier.SystemBarrierPrefix+quotas.StoragePrefix)
	return ij.im.core.quotaManager.Invalidate(ctx, quotaPath)
}

func (ij *invalidationJob) auditInvalidation(ctx context.Context) error {
	if ij.nsUUID != namespace.RootNamespaceUUID {
		ij.im.dispacherLogger.Warn("skipping invalidating audit table in non-root namespace", "ns", ij.nsUUID, "key", ij.nsKey)
		return nil
	}

	return ij.im.core.invalidateAudits(ctx)
}

func (ij *invalidationJob) legacyMountInvalidation(ctx context.Context) error {
	if err := ij.im.core.reloadLegacyMounts(ctx, ij.nsKey); err != nil {
		return fmt.Errorf("unable to invalidate legacy mount for key %q in namespace %q: %w", ij.nsKey, ij.nsUUID, err)
	}

	return nil
}

func (ij *invalidationJob) transactionalMountInvalidation(ctx context.Context) error {
	if err := ij.im.core.reloadMount(ctx, ij.nsKey); err != nil {
		return fmt.Errorf("unable to invalidate mount for key %q in namespace %q: %w", ij.nsKey, ij.nsUUID, err)
	}

	return nil
}

func (ij *invalidationJob) loginMFAInvalidation(ctx context.Context, ns *namespace.Namespace) error {
	if err := ij.im.core.loginMFABackend.invalidate(ctx, ns, ij.nsKey); err != nil {
		return fmt.Errorf("unable to invalidate login MFA config for key %q in namespace %q: %w", ij.nsKey, ij.nsUUID, err)
	}

	return nil
}

func (ij *invalidationJob) OnFailure(err error) {
	// Decide if we need to restart the core.
	if ij.quitContext.Err() != nil {
		return
	}

	if !ij.fatal {
		return
	}

	// This was a fatal failure; dispatch a restart.
	ij.im.dispacherLogger.Error("fatal failure dispatching invalidation; restarting core", "key", ij.key, "err", err)
	ij.im.core.restart()
}

func (im *invalidationManager) Add(key ...string) {
	// Skip invalidations if we're not enabled yet.
	if !im.enabled.Load() {
		return
	}

	// Skip invalidations if we're not the standby. The InmemHA backend in
	// particular dispatches invalidations on every node which isn't
	// necessary as the active is expected to invalidate itself in the course
	// of writing the data.
	if !im.core.standby.Load() {
		return
	}

	// Likewise if we're sealed, ignore the invalidation.
	if im.core.Sealed() {
		return
	}

	// Add the keys.
	im.pendingLock.Lock()
	im.pending = append(im.pending, key...)
	im.pendingLock.Unlock()

	// Notify the processor.
	select {
	case im.pendingNotify <- struct{}{}:
	default:
	}
}

func (im *invalidationManager) splitNamespaceFromKey(key string) (string, string) {
	namespaceUUID := namespace.RootNamespaceUUID
	namespacedKey := key

	if keySuffix, ok := strings.CutPrefix(key, namespaceBarrierPrefix); ok {
		namespaceUUID, namespacedKey, _ = strings.Cut(keySuffix, "/")
	}

	return namespaceUUID, namespacedKey
}
