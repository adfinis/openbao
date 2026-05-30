// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package policy

import (
	"context"
	"errors"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/vault/barrier"
	vaultidentity "github.com/openbao/openbao/vault/identity"
)

type testPolicyStoreCore struct {
	view barrier.View
}

func (c *testPolicyStoreCore) NamespaceByID(context.Context, string) (*namespace.Namespace, error) {
	return namespace.RootNamespace, nil
}

func (c *testPolicyStoreCore) IdentityStore() *vaultidentity.IdentityStore {
	return nil
}

func (c *testPolicyStoreCore) NamespaceView(*namespace.Namespace) barrier.View {
	return c.view
}

func testPolicyStore(t *testing.T) (*Store, barrier.View) {
	t.Helper()

	view := barrier.NewView(new(logical.InmemStorage), "")
	store, err := NewStore(namespace.RootContext(context.Background()), &testPolicyStoreCore{view: view}, view, logical.StaticSystemView{}, log.NewNullLogger())
	if err != nil {
		t.Fatal(err)
	}

	return store, view
}

func TestStoreLoadDefaultPoliciesAllowReadOnlyExistingPolicies(t *testing.T) {
	ctx := namespace.RootContext(context.Background())
	store, view := testPolicyStore(t)

	if err := store.LoadDefaultPolicies(ctx); err != nil {
		t.Fatal(err)
	}

	staleDefaultPolicy := `path "sys/health" { capabilities = ["read"] }`
	entry, err := logical.StorageEntryJSON(defaultPolicyName, &Entry{
		Version:     2,
		DataVersion: 1,
		Raw:         staleDefaultPolicy,
		Type:        TypeACL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := view.SubView(barrier.SystemBarrierPrefix+ACLSubPath).Put(ctx, entry); err != nil {
		t.Fatal(err)
	}
	store.PurgeCache()
	view.SetReadOnlyErr(logical.ErrReadOnly)

	if err := store.LoadDefaultPoliciesAllowReadOnly(ctx); err != nil {
		t.Fatal(err)
	}

	pol, err := store.GetPolicy(ctx, defaultPolicyName, TypeACL)
	if err != nil {
		t.Fatal(err)
	}
	if pol == nil {
		t.Fatal("default policy is missing")
	}
	if pol.Raw != staleDefaultPolicy {
		t.Fatalf("default policy was unexpectedly rewritten: %q", pol.Raw)
	}
}

func TestStoreLoadDefaultPoliciesAllowReadOnlyMissingPolicy(t *testing.T) {
	ctx := namespace.RootContext(context.Background())
	store, view := testPolicyStore(t)

	if err := store.LoadACLPolicy(ctx, ResponseWrappingPolicyName, ResponseWrappingPolicy); err != nil {
		t.Fatal(err)
	}
	view.SetReadOnlyErr(logical.ErrReadOnly)

	err := store.LoadDefaultPoliciesAllowReadOnly(ctx)
	if !errors.Is(err, logical.ErrReadOnly) {
		t.Fatalf("expected readonly error, got %v", err)
	}
}
