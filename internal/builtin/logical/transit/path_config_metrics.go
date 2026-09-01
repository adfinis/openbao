// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package transit

import (
	"context"
	"fmt"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	httpPathMetricsConfig    = "config/metrics"
	storagePathMetricsConfig = "config/metrics"
)

func (b *backend) pathConfigMetrics() *framework.Path {
	return &framework.Path{
		Pattern: httpPathMetricsConfig,

		DisplayAttrs: &framework.DisplayAttributes{
			OperationPrefix: operationPrefixTransit,
		},

		Fields: map[string]*framework.FieldSchema{
			"key_count_metric_enabled": {
				Type: framework.TypeBool,
				Description: `Whether to enable the metric counting the total number of keys 
in this transit engine instance`,
				Default: false,
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathConfigMetricsWrite,
				DisplayAttrs: &framework.DisplayAttributes{
					OperationVerb:   "configure",
					OperationSuffix: "metrics",
				},
			},
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathConfigMetricsRead,
				DisplayAttrs: &framework.DisplayAttributes{
					OperationSuffix: "metrics-configuration",
				},
			},
		},

		HelpSynopsis:    pathConfigMetricsHelpSyn,
		HelpDescription: pathConfigMetricsHelpDesc,
	}
}

func (b *backend) writeConfigMetrics(ctx context.Context, req *logical.Request, cfg *configMetrics) error {
	entry, err := logical.StorageEntryJSON(storagePathMetricsConfig, cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal metrics configuration: %w", err)
	}

	return req.Storage.Put(ctx, entry)
}

func respondConfigMetrics(cfg *configMetrics) *logical.Response {
	return &logical.Response{
		Data: map[string]any{
			"key_count_metric_enabled": cfg.KeyCountMetricEnabled,
		},
	}
}

func (b *backend) pathConfigMetricsWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	b.metricsConfigMutex.Lock()
	defer b.metricsConfigMutex.Unlock()

	txRollback, err := logical.StartTxStorage(ctx, req)
	if err != nil {
		return nil, err
	}
	defer txRollback()

	keyCountMetricEnabled := d.Get("key_count_metric_enabled").(bool)

	cfg := b.metricsConfig.Load()
	if cfg == nil {
		cfg = &configMetrics{}
	}

	modified := false

	if cfg.KeyCountMetricEnabled != keyCountMetricEnabled {
		cfg.KeyCountMetricEnabled = keyCountMetricEnabled
		modified = true
	}

	if modified {
		if err := b.writeConfigMetrics(ctx, req, cfg); err != nil {
			return nil, err
		}
	}

	if err := logical.EndTxStorage(ctx, req); err != nil {
		return nil, err
	}

	if modified {
		b.metricsConfig.Store(cfg)
		err = b.initMetrics(ctx, req.Storage)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize metrics, try reloading the mount: %w", err)
		}
	}

	return respondConfigMetrics(cfg), nil
}

func (b *backend) pathConfigMetricsRead(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	cfg := b.metricsConfig.Load()
	if cfg == nil {
		cfg = &configMetrics{}
	}

	return respondConfigMetrics(cfg), nil
}

const pathConfigMetricsHelpSyn = `Configuration of engine metrics`

const pathConfigMetricsHelpDesc = `
This path is used to configure engine metrics. Currently, this supportes
toggling the total key count metric.
`
