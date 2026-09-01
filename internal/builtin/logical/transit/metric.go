// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package transit

import (
	"context"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/logical"
)

func (b *backend) keyCountMetricEnabled() bool {
	cfg := b.metricsConfig.Load()
	return cfg != nil && cfg.KeyCountMetricEnabled
}

func (b *backend) emitMetrics() {
	if !b.keyCountMetricEnabled() {
		return
	}

	metrics.SetGaugeWithLabels([]string{"secrets", "transit", "key_count"}, float32(b.counter.Load()), []metrics.Label{{
		Name:  "backend_uuid",
		Value: b.backendUUID,
	}})
}

func (b *backend) incrementKeyCount() {
	if !b.keyCountMetricEnabled() {
		return
	}

	b.counter.Add(1)
}

func (b *backend) decrementKeyCount() {
	if !b.keyCountMetricEnabled() {
		return
	}

	twosComplementOfMinusOne := ^uint64(0) // aka: all bits 1
	b.counter.Add(twosComplementOfMinusOne)
}

type configMetrics struct {
	KeyCountMetricEnabled bool `json:"key_count_metric_enabled"`
}

func (b *backend) loadMetricsConfig(ctx context.Context, s logical.Storage) error {
	entry, err := s.Get(ctx, storagePathMetricsConfig)
	if err != nil {
		b.metricsConfig.Store(&configMetrics{})
		return err
	}
	if entry == nil {
		b.metricsConfig.Store(&configMetrics{})
		return nil
	}

	config := &configMetrics{}
	err = entry.DecodeJSON(config)
	b.metricsConfig.Store(config)
	return err
}

func (b *backend) initMetrics(ctx context.Context, storage logical.Storage) error {
	if !b.keyCountMetricEnabled() {
		b.counter.Store(0)
		return nil
	}

	// For this to be correct, we must have a mount wide lock
	// - init would grab an exclusive look
	// - create or delete operations would grab a shared lock
	//
	// Essentially initializing the counter has to be an atomic operation.
	// - when transactions are available, we have to hood the look as long as we
	//   have acquire the transaction and reset the counter to 0
	// - otherwise we need to hold the lock for the whole counting operation
	//
	// Anyway, lets ignore this for now: After enabling the metric, users can
	// simply reload the mount, which would guarantee correctness as the counting
	// happens during initialization - before any requests are routed (or restart
	// the node).

	if txStorage, ok := storage.(logical.TransactionalStorage); ok {
		tx, err := txStorage.BeginReadOnlyTx(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		storage = tx
	}
	b.counter.Store(0)

	data, err := storage.List(ctx, "policy/")
	if err != nil {
		return err
	}

	for _, key := range data {
		if key != "import/" {
			b.counter.Add(1)
		}
	}

	b.emitMetrics()

	return nil
}
