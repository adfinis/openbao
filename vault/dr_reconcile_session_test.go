// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyReconcileFailure_Stalled(t *testing.T) {
	err := errors.New("reconcile failure [stalled]: stalled without task progress for 2m0s")
	class := classifyReconcileFailure(err)
	if class != drReconcileFailureStalled {
		t.Fatalf("expected stalled class, got %q", class)
	}
}

func TestWrapReconcileFailure_PreservesBudgetClass(t *testing.T) {
	err := errors.New("budget_exceeded: reconcile RPC bytes exceeded (10 > 5)")
	wrapped := wrapReconcileFailure(drReconcileFailureDecodeExhausted, "unit-test", err)
	if !strings.Contains(strings.ToLower(wrapped.Error()), "reconcile failure [budget_exceeded]") {
		t.Fatalf("expected budget_exceeded wrapper class, got: %v", wrapped)
	}
}

func TestFallbackTriggerWindow_LagAndFailures(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	_, _ = rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-1", core.logger)

	sec.fallbackEnabled = true
	sec.fallbackStall = 30 * time.Second
	sec.fallbackFailureThreshold = 3
	sec.fallbackMinLagEntries = 1000
	sec.fallbackCooldown = 5 * time.Minute
	sec.fallbackWindow = 10 * time.Minute
	sec.fallbackMaxPerHour = 2

	sec.setLastAppliedIndex(100)
	sec.lastAppliedAt.Store(time.Now().Add(-2 * time.Minute).Unix())
	sec.primaryIndex.Store(5000)

	sec.recordFallbackFailure(drReconcileFailureBudgetExceeded)
	sec.recordFallbackFailure(drReconcileFailureStalled)
	sec.recordFallbackFailure(drReconcileFailureDecodeExhausted)

	if !sec.shouldTriggerFallback(drReconcileFailureBudgetExceeded) {
		t.Fatal("expected fallback trigger after threshold failures with sufficient lag")
	}
}

func TestFallbackTriggerWindow_RespectsCooldown(t *testing.T) {
	core, _, _ := TestCoreUnsealed(t)
	replSalt := make([]byte, 32)
	_, _ = rand.Read(replSalt)
	sec := newDRReplicationSecondary(core, replSalt, "rel-1", core.logger)

	sec.fallbackEnabled = true
	sec.fallbackStall = 30 * time.Second
	sec.fallbackFailureThreshold = 1
	sec.fallbackMinLagEntries = 10
	sec.fallbackCooldown = 10 * time.Minute
	sec.fallbackWindow = 10 * time.Minute
	sec.fallbackMaxPerHour = 2

	sec.setLastAppliedIndex(100)
	sec.lastAppliedAt.Store(time.Now().Add(-2 * time.Minute).Unix())
	sec.primaryIndex.Store(10000)
	sec.recordFallbackFailure(drReconcileFailureBudgetExceeded)
	sec.fallbackLastAt.Store(time.Now().Unix())

	if sec.shouldTriggerFallback(drReconcileFailureBudgetExceeded) {
		t.Fatal("expected cooldown to block fallback trigger")
	}
}
