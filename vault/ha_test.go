// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbao/openbao/helper/locking"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrabLockOrStopped is a non-deterministic test to detect deadlocks in the
// grabLockOrStopped function. This test starts a bunch of workers which
// continually lock/unlock and rlock/runlock the same RWMutex. Each worker also
// starts a goroutine which closes the stop channel 1/2 the time, which races
// with acquisition of the lock.
func TestGrabLockOrStop(t *testing.T) {
	// Stop the test early if we deadlock.
	const (
		workers      = 100
		testDuration = time.Second
		testTimeout  = 10 * testDuration
	)
	done := make(chan struct{})
	defer close(done)
	lockCount := atomic.Uint32{}
	go func() {
		select {
		case <-done:
		case <-time.After(testTimeout):
			panic(fmt.Sprintf("deadlock after %d lock count",
				lockCount.Load()))
		}
	}()

	// lock is locked/unlocked and rlocked/runlocked concurrently.
	var lock sync.RWMutex
	start := time.Now()

	// workerWg is used to wait until all workers exit.
	var workerWg sync.WaitGroup
	workerWg.Add(workers)

	// Start a bunch of worker goroutines.
	for g := range workers {
		go func() {
			defer workerWg.Done()
			for time.Since(start) < testDuration {
				stop := make(chan struct{})

				// closerWg waits until the closer goroutine exits before we do
				// another iteration. This makes sure goroutines don't pile up.
				var closerWg sync.WaitGroup
				closerWg.Go(func() {
					// Close the stop channel half the time.
					if rand.Int()%2 == 0 {
						close(stop)
					}
				})

				// Half the goroutines lock/unlock and the other half rlock/runlock.
				if g%2 == 0 {
					if !grabLockOrStop(lock.Lock, lock.Unlock, stop) {
						lock.Unlock()
					}
				} else {
					if !grabLockOrStop(lock.RLock, lock.RUnlock, stop) {
						lock.RUnlock()
					}
				}

				closerWg.Wait()

				// This lets us know how many lock/unlock and rlock/runlock have
				// happened if there's a deadlock.
				lockCount.Add(1)
			}
		}()
	}
	workerWg.Wait()
}

func TestRunStandbyGrabStateLockTimeout(t *testing.T) {
	previousDuration := DefaultMaxRequestDuration
	DefaultMaxRequestDuration = 10 * time.Millisecond
	defer func() {
		DefaultMaxRequestDuration = previousDuration
	}()

	c := &Core{
		stateLock: &locking.SyncRWMutex{},
	}
	c.stateLock.Lock()

	done := make(chan error, 1)
	go func() {
		done <- c.runStandbyGrabStateLock(make(chan struct{}))
	}()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		c.stateLock.Unlock()
		t.Fatal("timed out waiting for standby state-lock acquisition to stop")
	}

	c.stateLock.Unlock()

	released := make(chan struct{})
	go func() {
		c.stateLock.Lock()
		c.stateLock.Unlock()
		close(released)
	}()

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for abandoned state-lock acquisition to release")
	}
}

func TestGrabStateLockOrStopDoesNotQueueWriter(t *testing.T) {
	c := &Core{
		stateLock: &locking.SyncRWMutex{},
	}
	c.stateLock.Lock()

	stopCh := make(chan struct{})
	done := make(chan bool, 1)
	go func() {
		done <- c.grabStateLockOrStop(stopCh, nil)
	}()

	time.Sleep(20 * time.Millisecond)
	close(stopCh)

	select {
	case stopped := <-done:
		require.True(t, stopped)
	case <-time.After(time.Second):
		c.stateLock.Unlock()
		t.Fatal("timed out waiting for state-lock acquisition to stop")
	}

	c.stateLock.Unlock()

	released := make(chan struct{})
	go func() {
		c.stateLock.Lock()
		c.stateLock.Unlock()
		close(released)
	}()

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for abandoned state-lock acquisition to release")
	}
}

func TestCoreRestart(t *testing.T) {
	t.Parallel()

	t.Run("active", func(t *testing.T) {
		t.Skip("there is a data-race in waitForLeadership: https://github.com/openbao/openbao/blob/5a93ec0549a88516a8a15e96ef74dadac8ed506f/vault/ha.go#L676-L687")
		testCoreRestart(t, 0)
	})
	t.Run("standby", func(t *testing.T) {
		testCoreRestart(t, 1)
	})
}

func testCoreRestart(t *testing.T, core int) {
	t.Parallel()

	c := NewTestCluster(t, &CoreConfig{}, &TestClusterOptions{
		NumCores: 2,
	})

	c.Start()
	defer c.Cleanup()

	TestWaitActive(t, c.Cores[0].Core)

	c.Cores[core].stateLock.RLock()
	activeContextDone := c.Cores[core].activeContext.Load().Done()
	c.Cores[core].stateLock.RUnlock()

	// trigger the restart
	c.Cores[core].restart()

	// wait until the active context is cancelled
	select {
	case <-activeContextDone:
	case <-time.NewTimer(10 * time.Second).C:
		t.Fatal("timeout while waiting for context to be cancelled")
	}

	// a new context should be started
	require.EventuallyWithT(t, func(t *assert.CollectT) {
		c.Cores[core].stateLock.RLock()
		defer c.Cores[core].stateLock.RUnlock()
		require.NotNil(t, c.Cores[core].activeContext.Load())
		require.Nil(t, c.Cores[core].activeContext.Load().Err())
	}, 10*time.Second, 10*time.Millisecond)
}
