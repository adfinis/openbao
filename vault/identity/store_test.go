// Copyright (c) 2026 OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

package identity

import (
	"context"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/sdk/v2/logical"
)

type readOnlyIdentityStorage struct {
	logical.InmemStorage
}

func (s *readOnlyIdentityStorage) Put(context.Context, *logical.StorageEntry) error {
	return logical.ErrReadOnly
}

func (s *readOnlyIdentityStorage) Delete(context.Context, string) error {
	return logical.ErrReadOnly
}

func TestIdentityStoreInitializeToleratesReadOnlyOIDCDefaults(t *testing.T) {
	ctx := namespace.RootContext(t.Context())
	store := &IdentityStore{logger: log.NewNullLogger()}
	store.views.Store(namespace.RootNamespaceUUID, &identityStoreNamespaceView{
		view: &logical.InmemStorage{},
	})

	err := store.initialize(ctx, &logical.InitializationRequest{
		Storage: &readOnlyIdentityStorage{},
	})
	if err != nil {
		t.Fatalf("initialize should tolerate read-only OIDC default resources: %v", err)
	}
}
