// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package vault

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openbao/openbao/command/server"

	logicalDb "github.com/openbao/openbao/builtin/logical/database"
	logicalKv "github.com/openbao/openbao/builtin/logical/kv"

	"github.com/openbao/openbao/builtin/plugin"

	"github.com/openbao/openbao/builtin/audit/syslog"

	"github.com/openbao/openbao/builtin/audit/file"
	"github.com/openbao/openbao/builtin/audit/socket"
	"github.com/stretchr/testify/require"

	"github.com/go-test/deep"
	"github.com/hashicorp/errwrap"
	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/hashicorp/go-uuid"
	"github.com/openbao/openbao/audit"
	"github.com/openbao/openbao/helper/identity/mfa"
	"github.com/openbao/openbao/helper/namespace"
	"github.com/openbao/openbao/helper/testhelpers/corehelpers"
	"github.com/openbao/openbao/internalshared/configutil"
	"github.com/openbao/openbao/sdk/v2/helper/consts"
	"github.com/openbao/openbao/sdk/v2/helper/jsonutil"
	"github.com/openbao/openbao/sdk/v2/helper/logging"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/sdk/v2/physical"
	"github.com/openbao/openbao/sdk/v2/physical/inmem"
	"github.com/openbao/openbao/version"
)

// invalidKey is used to test Unseal
var invalidKey = []byte("abcdefghijklmnopqrstuvwxyz")[:17]

// TestNewCore_configureAuditBackends ensures that we are able to configure the
// supplied audit backends when getting a NewCore.
func TestNewCore_configureAuditBackends(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		backends map[string]audit.Factory
	}{
		"none": {
			backends: nil,
		},
		"file": {
			backends: map[string]audit.Factory{
				"file": file.Factory,
			},
		},
		"socket": {
			backends: map[string]audit.Factory{
				"socket": socket.Factory,
			},
		},
		"syslog": {
			backends: map[string]audit.Factory{
				"syslog": syslog.Factory,
			},
		},
		"all": {
			backends: map[string]audit.Factory{
				"file":   file.Factory,
				"socket": socket.Factory,
				"syslog": syslog.Factory,
			},
		},
	}

	for name, tc := range tests {
		name := name
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			core := &Core{}
			require.Len(t, core.auditBackends, 0)
			core.configureAuditBackends(tc.backends)
			require.Len(t, core.auditBackends, len(tc.backends))
			for k := range tc.backends {
				require.Contains(t, core.auditBackends, k)
			}
		})
	}
}

// TestNewCore_configureCredentialsBackends ensures that we are able to configure the
// supplied credential backends, in addition to defaults, when getting a NewCore.
func TestNewCore_configureCredentialsBackends(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		backends map[string]logical.Factory
	}{
		"none": {
			backends: nil,
		},
		"plugin": {
			backends: map[string]logical.Factory{
				"plugin": plugin.Factory,
			},
		},
	}

	for name, tc := range tests {
		name := name
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			core := &Core{}
			require.Len(t, core.credentialBackends, 0)
			core.configureCredentialsBackends(tc.backends, corehelpers.NewTestLogger(t))
			require.GreaterOrEqual(t, len(core.credentialBackends), len(tc.backends)+1) // token + ent
			for k := range tc.backends {
				require.Contains(t, core.credentialBackends, k)
			}
		})
	}
}

// TestNewCore_configureLogicalBackends ensures that we are able to configure the
// supplied logical backends, in addition to defaults, when getting a NewCore.
func TestNewCore_configureLogicalBackends(t *testing.T) {
	t.Parallel()

	// configureLogicalBackends will add some default backends for us:
	// cubbyhole
	// identity
	// kv
	// system

	tests := map[string]struct {
		backends               map[string]logical.Factory
		adminNamespacePath     string
		expectedNonEntBackends int
	}{
		"none": {
			backends:               nil,
			expectedNonEntBackends: 0,
		},
		"database": {
			backends: map[string]logical.Factory{
				"database": logicalDb.Factory,
			},
			adminNamespacePath:     "foo",
			expectedNonEntBackends: 5, // database + defaults
		},
		"kv": {
			backends: map[string]logical.Factory{
				"kv": logicalKv.Factory,
			},
			adminNamespacePath:     "foo",
			expectedNonEntBackends: 4, // kv + defaults (kv is a default)
		},
		"plugin": {
			backends: map[string]logical.Factory{
				"plugin": plugin.Factory,
			},
			adminNamespacePath:     "foo",
			expectedNonEntBackends: 5, // plugin + defaults
		},
		"all": {
			backends: map[string]logical.Factory{
				"database": logicalDb.Factory,
				"kv":       logicalKv.Factory,
				"plugin":   plugin.Factory,
			},
			adminNamespacePath:     "foo",
			expectedNonEntBackends: 6, // database, plugin + defaults
		},
	}

	for name, tc := range tests {
		name := name
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			core := &Core{}
			require.Len(t, core.logicalBackends, 0)
			core.configureLogicalBackends(tc.backends, corehelpers.NewTestLogger(t), tc.adminNamespacePath)
			require.GreaterOrEqual(t, len(core.logicalBackends), tc.expectedNonEntBackends)
			require.Contains(t, core.logicalBackends, mountTypeKV)
			require.Contains(t, core.logicalBackends, mountTypeCubbyhole)
			require.Contains(t, core.logicalBackends, mountTypeSystem)
			require.Contains(t, core.logicalBackends, mountTypeIdentity)
			for k := range tc.backends {
				require.Contains(t, core.logicalBackends, k)
			}
		})
	}
}

// TestNewCore_configureLogRequestLevel ensures that we are able to configure the
// supplied logging level when getting a NewCore.
func TestNewCore_configureLogRequestLevel(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		level         string
		expectedLevel log.Level
	}{
		"none": {
			level:         "",
			expectedLevel: log.NoLevel,
		},
		"trace": {
			level:         "trace",
			expectedLevel: log.Trace,
		},
		"debug": {
			level:         "debug",
			expectedLevel: log.Debug,
		},
		"info": {
			level:         "info",
			expectedLevel: log.Info,
		},
		"warn": {
			level:         "warn",
			expectedLevel: log.Warn,
		},
		"error": {
			level:         "error",
			expectedLevel: log.Error,
		},
		"bad": {
			level:         "foo",
			expectedLevel: log.NoLevel,
		},
	}

	for name, tc := range tests {
		name := name
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// We need to supply a logger, as configureLogRequestsLevel emits
			// warnings to the logs in certain circumstances.
			core := &Core{
				logger: corehelpers.NewTestLogger(t),
			}
			core.configureLogRequestsLevel(tc.level)
			require.Equal(t, tc.expectedLevel, log.Level(core.logRequestsLevel.Load()))
		})
	}
}

// TestNewCore_configureListeners tests that we are able to configure listeners
// on a NewCore via config.
func TestNewCore_configureListeners(t *testing.T) {
	// We would usually expect CoreConfig to come from server.NewConfig().
	// However, we want to fiddle to give us some granular control over the config.
	tests := map[string]struct {
		config            *CoreConfig
		expectedListeners []*ListenerCustomHeaders
	}{
		"nil-listeners": {
			config: &CoreConfig{
				RawConfig: &server.Config{
					SharedConfig: &configutil.SharedConfig{},
				},
			},
			expectedListeners: nil,
		},
		"listeners-empty": {
			config: &CoreConfig{
				RawConfig: &server.Config{
					SharedConfig: &configutil.SharedConfig{
						Listeners: []*configutil.Listener{},
					},
				},
			},
			expectedListeners: nil,
		},
		"listeners-some": {
			config: &CoreConfig{
				RawConfig: &server.Config{
					SharedConfig: &configutil.SharedConfig{
						Listeners: []*configutil.Listener{
							{Address: "foo"},
						},
					},
				},
			},
			expectedListeners: []*ListenerCustomHeaders{
				{Address: "foo"},
			},
		},
	}

	for name, tc := range tests {
		name := name
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// We need to init some values ourselves, usually CreateCore does this for us.
			logger := corehelpers.NewTestLogger(t)
			backend, err := inmem.NewInmem(nil, logger)
			require.NoError(t, err)
			storage := &logical.InmemStorage{}
			core := &Core{
				clusterListener:      new(atomic.Value),
				customListenerHeader: new(atomic.Value),
				uiConfig:             NewUIConfig(false, backend, storage),
			}

			err = core.configureListeners(tc.config)
			require.NoError(t, err)
			switch tc.expectedListeners {
			case nil:
				require.Nil(t, core.customListenerHeader.Load())
			default:
				for i, v := range core.customListenerHeader.Load().([]*ListenerCustomHeaders) {
					require.Equal(t, v.Address, tc.config.RawConfig.Listeners[i].Address)
				}
			}
		})
	}
}

func TestNewCore_badRedirectAddr(t *testing.T) {
	logger = logging.NewVaultLogger(log.Trace)

	inm, err := inmem.NewInmem(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	conf := &CoreConfig{
		RedirectAddr: "127.0.0.1:8200",
		Physical:     inm,
	}
	_, err = NewCore(conf)
	if err == nil {
		t.Fatal("should error")
	}
}

func TestSealConfig_Invalid(t *testing.T) {
	s := &SealConfig{
		SecretShares:    2,
		SecretThreshold: 1,
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("expected err")
	}
}

// TestCore_HasVaultVersion checks that versionHistory is correct and initialized
// after a core has been unsealed.
func TestCore_HasVaultVersion(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)
	if c.versionHistory == nil {
		t.Fatal("Version timestamps for core were not initialized for a new core")
	}
	versionEntry, ok := c.versionHistory[version.Version]
	if !ok {
		t.Fatalf("%s upgrade time not found", version.Version)
	}

	upgradeTime := versionEntry.TimestampInstalled

	if upgradeTime.After(time.Now()) || upgradeTime.Before(time.Now().Add(-1*time.Hour)) {
		t.Fatal("upgrade time isn't within reasonable bounds of new core initialization. " +
			fmt.Sprintf("time is: %+v, upgrade time is %+v", time.Now(), upgradeTime))
	}
}

func TestCore_Unseal_MultiShare(t *testing.T) {
	c := TestCore(t)

	_, err := TestCoreUnseal(c, invalidKey)
	if err != ErrNotInit {
		t.Fatalf("err: %v", err)
	}

	sealConf := &SealConfig{
		SecretShares:    5,
		SecretThreshold: 3,
	}
	res, err := c.Initialize(namespace.RootContext(nil), &InitParams{
		BarrierConfig:  sealConf,
		RecoveryConfig: nil,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if !c.Sealed() {
		t.Fatal("should be sealed")
	}

	if prog, _ := c.SecretProgress(true); prog != 0 {
		t.Fatalf("bad progress: %d", prog)
	}

	for i := 0; i < 5; i++ {
		unseal, err := TestCoreUnseal(c, res.SecretShares[i])
		if err != nil {
			t.Fatalf("err: %v", err)
		}

		// Ignore redundant
		_, err = TestCoreUnseal(c, res.SecretShares[i])
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if i >= 2 {
			if !unseal {
				t.Fatal("should be unsealed")
			}
			if prog, _ := c.SecretProgress(true); prog != 0 {
				t.Fatalf("bad progress: %d", prog)
			}
		} else {
			if unseal {
				t.Fatal("should not be unsealed")
			}
			if prog, _ := c.SecretProgress(true); prog != i+1 {
				t.Fatalf("bad progress: %d", prog)
			}
		}
	}

	if c.Sealed() {
		t.Fatal("should not be sealed")
	}

	err = c.Seal(res.RootToken)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ignore redundant
	err = c.Seal(res.RootToken)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if !c.Sealed() {
		t.Fatal("should be sealed")
	}
}

// TestCore_UseSSCTokenToggleOn will check that the root SSC
// token can be used even when disableSSCTokens is toggled on
func TestCore_UseSSCTokenToggleOn(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)
	c.disableSSCTokens = true
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: root,
	}
	ctx := namespace.RootContext(nil)
	resp, err := c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(ctx, req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Data == nil {
		t.Fatalf("bad: %#v", resp)
	}
	if resp.Secret.TTL != time.Hour {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Data["foo"] != "bar" {
		t.Fatalf("bad: %#v", resp.Data)
	}
}

// TestCore_UseNonSSCTokenToggleOff will check that the root
// non-SSC token can be used even when disableSSCTokens is toggled
// off.
func TestCore_UseNonSSCTokenToggleOff(t *testing.T) {
	coreConfig := &CoreConfig{
		DisableSSCTokens: true,
	}
	c, _, root := TestCoreUnsealedWithConfig(t, coreConfig)
	if len(root) > TokenLength+OldTokenPrefixLength || !strings.HasPrefix(root, consts.LegacyServiceTokenPrefix) {
		t.Fatalf("token is not an old token type: %s, %d", root, len(root))
	}
	c.disableSSCTokens = false
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: root,
	}
	ctx := namespace.RootContext(nil)
	resp, err := c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(ctx, req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Data == nil {
		t.Fatalf("bad: %#v", resp)
	}
	if resp.Secret.TTL != time.Hour {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Data["foo"] != "bar" {
		t.Fatalf("bad: %#v", resp.Data)
	}
}

func TestCore_Unseal_Single(t *testing.T) {
	c := TestCore(t)

	_, err := TestCoreUnseal(c, invalidKey)
	if err != ErrNotInit {
		t.Fatalf("err: %v", err)
	}

	sealConf := &SealConfig{
		SecretShares:    1,
		SecretThreshold: 1,
	}
	res, err := c.Initialize(namespace.RootContext(nil), &InitParams{
		BarrierConfig:  sealConf,
		RecoveryConfig: nil,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if !c.Sealed() {
		t.Fatal("should be sealed")
	}

	if prog, _ := c.SecretProgress(true); prog != 0 {
		t.Fatalf("bad progress: %d", prog)
	}

	unseal, err := TestCoreUnseal(c, res.SecretShares[0])
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if !unseal {
		t.Fatal("should be unsealed")
	}
	if prog, _ := c.SecretProgress(true); prog != 0 {
		t.Fatalf("bad progress: %d", prog)
	}

	if c.Sealed() {
		t.Fatal("should not be sealed")
	}
}

func TestCore_Route_Sealed(t *testing.T) {
	c := TestCore(t)
	sealConf := &SealConfig{
		SecretShares:    1,
		SecretThreshold: 1,
	}

	ctx := namespace.RootContext(nil)

	// Should not route anything
	req := &logical.Request{
		Operation: logical.ReadOperation,
		Path:      "sys/mounts",
	}
	_, err := c.HandleRequest(ctx, req)
	if err != consts.ErrSealed {
		t.Fatalf("err: %v", err)
	}

	res, err := c.Initialize(ctx, &InitParams{
		BarrierConfig:  sealConf,
		RecoveryConfig: nil,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	unseal, err := TestCoreUnseal(c, res.SecretShares[0])
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !unseal {
		t.Fatal("should be unsealed")
	}

	// Should not error after unseal
	req.ClientToken = res.RootToken
	_, err = c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
}

// Attempt to unseal after doing a first seal
func TestCore_SealUnseal(t *testing.T) {
	c, keys, root := TestCoreUnsealed(t)
	if err := c.Seal(root); err != nil {
		t.Fatalf("err: %v", err)
	}
	for i, key := range keys {
		unseal, err := TestCoreUnseal(c, key)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if i+1 == len(keys) && !unseal {
			t.Fatal("err: should be unsealed")
		}
	}
}

// TestCore_LoadLoginMFAConfigs verifies proper storage of the MFA and MFA Enforcement configs
// looking at the storage before and after saving the configs
func TestCore_LoadLoginMFAConfigs(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)
	ctx := namespace.RootContext(context.Background())
	ns1 := &namespace.Namespace{Path: "ns1/"}
	TestCoreCreateNamespaces(t, c, ns1)

	// prepare views
	nsView := NamespaceView(c.barrier, ns1)
	mfaConfigBarrierView := nsView.SubView(systemBarrierPrefix).SubView(loginMFAConfigPrefix)
	mfaEnforcementConfigBarrierView := nsView.SubView(systemBarrierPrefix).SubView(mfaLoginEnforcementPrefix)

	// verify empty storage
	mfaConfigKeys, err := mfaConfigBarrierView.List(ctx, "")
	require.NoError(t, err)
	require.Empty(t, mfaConfigKeys)

	mfaEnforcementConfigKeys, err := mfaEnforcementConfigBarrierView.List(ctx, "")
	require.NoError(t, err)
	require.Empty(t, mfaConfigKeys)

	// store configs
	mConfig := &mfa.Config{Name: "mConfig", NamespaceID: ns1.ID, ID: "mConfigID", Type: mfaMethodTypeTOTP}
	err = c.loginMFABackend.putMFAConfigByID(namespace.ContextWithNamespace(ctx, ns1), mConfig)
	require.NoError(t, err)

	eConfig := &mfa.MFAEnforcementConfig{Name: "eConfig", NamespaceID: ns1.ID, ID: "eConfigID"}
	err = c.loginMFABackend.putMFALoginEnforcementConfig(ctx, eConfig)
	require.NoError(t, err)

	// check for errors when loading
	err = c.loadLoginMFAConfigs(ctx)
	require.NoError(t, err)

	// verify storage keys after
	mfaConfigKeys, err = mfaConfigBarrierView.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, mfaConfigKeys, 1)
	require.Equal(t, "mConfigID", mfaConfigKeys[0])

	mfaEnforcementConfigKeys, err = mfaEnforcementConfigBarrierView.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, mfaEnforcementConfigKeys, 1)
	require.Equal(t, "eConfigID", mfaEnforcementConfigKeys[0])
}

// TestCore_RunLockedUserUpdatesForStaleEntry tests that
// stale locked user entries get deleted upon unseal.
func TestCore_RunLockedUserUpdatesForStaleEntry(t *testing.T) {
	core, keys, root := TestCoreUnsealed(t)

	ctx := namespace.RootContext(context.Background())
	testNamespace := &namespace.Namespace{Path: "test"}
	TestCoreCreateNamespaces(t, core, testNamespace)

	barrier := NamespaceView(core.barrier, testNamespace).SubView(coreLockedUsersPath).SubView("mountAccessor1/")
	// cleanup
	defer barrier.Delete(ctx, "aliasName1")

	// create invalid entry in storage to test stale entries get deleted on unseal
	// last failed login time for this path is 1970-01-01 00:00:00 +0000 UTC
	// since user lockout configurations are not configured, lockout duration will
	// be set to default (15m) internally
	compressedBytes, err := jsonutil.EncodeJSONAndCompress(int(time.Unix(0, 0).Unix()), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create an entry
	entry := &logical.StorageEntry{
		Key:   "aliasName1",
		Value: compressedBytes,
	}

	// Write to the physical backend
	err = barrier.Put(ctx, entry)
	if err != nil {
		t.Fatalf("failed to write invalid locked user entry, err: %v", err)
	}

	// seal and unseal vault
	if err := core.Seal(root); err != nil {
		t.Fatalf("err: %v", err)
	}
	for i, key := range keys {
		unseal, err := TestCoreUnseal(core, key)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if i+1 == len(keys) && !unseal {
			t.Fatal("err: should be unsealed")
		}
	}

	// locked user entry must be deleted upon unseal as it is stale
	lastFailedLoginRaw, err := barrier.Get(ctx, "aliasName1")
	if err != nil {
		t.Fatal(err)
	}
	if lastFailedLoginRaw != nil {
		t.Fatal("err: stale locked user entry exists")
	}
}

// TestCore_RunLockedUserUpdatesForValidEntry tests that
// valid locked user entries do not get removed on unseal.
// Also verifies userFailedLoginInfo map getting updated.
func TestCore_RunLockedUserUpdatesForValidEntry(t *testing.T) {
	core, keys, root := TestCoreUnsealed(t)

	ctx := namespace.RootContext(context.Background())
	testNamespace := &namespace.Namespace{Path: "test"}
	TestCoreCreateNamespaces(t, core, testNamespace)

	barrier := NamespaceView(core.barrier, testNamespace).SubView(coreLockedUsersPath).SubView("mountAccessor1/")
	// cleanup
	defer barrier.Delete(ctx, "aliasName1")

	// create valid storage entry for locked user
	lastFailedLoginTime := int(time.Now().Unix())

	compressedBytes, err := jsonutil.EncodeJSONAndCompress(lastFailedLoginTime, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create an entry
	entry := &logical.StorageEntry{
		Key:   "aliasName1",
		Value: compressedBytes,
	}

	// Write to the physical backend
	err = barrier.Put(ctx, entry)
	if err != nil {
		t.Fatalf("failed to write invalid locked user entry, err: %v", err)
	}

	// seal and unseal vault
	if err := core.Seal(root); err != nil {
		t.Fatalf("err: %v", err)
	}
	for i, key := range keys {
		unseal, err := TestCoreUnseal(core, key)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if i+1 == len(keys) && !unseal {
			t.Fatal("err: should be unsealed")
		}
	}

	// locked user entry must exist as it is still valid
	existingEntry, err := barrier.Get(ctx, "aliasName1")
	if err != nil {
		t.Fatal(err)
	}
	if existingEntry == nil {
		t.Fatal("err: entry must exist for locked user in storage")
	}

	// userFailedLoginInfo map should have the correct information for locked user
	loginUserInfoKey := FailedLoginUser{
		aliasName:     "aliasName1",
		mountAccessor: "mountAccessor1",
	}

	failedLoginInfoFromMap := core.LocalGetUserFailedLoginInfo(ctx, loginUserInfoKey)
	if failedLoginInfoFromMap == nil {
		t.Fatal("err: entry must exist for locked user in userFailedLoginInfo map")
	}
	if failedLoginInfoFromMap.lastFailedLoginTime != lastFailedLoginTime {
		t.Fatal("err: incorrect failed login time information for locked user updated in userFailedLoginInfo map")
	}
	if int(failedLoginInfoFromMap.count) != configutil.UserLockoutThresholdDefault {
		t.Fatal("err: incorrect failed login count information for locked user updated in userFailedLoginInfo map")
	}
}

// Attempt to shutdown after unseal
func TestCore_Shutdown(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)
	if err := c.Shutdown(); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !c.Sealed() {
		t.Fatal("wasn't sealed")
	}
}

// verify the channel returned by ShutdownDone is closed after Finalize
func TestCore_ShutdownDone(t *testing.T) {
	c := TestCoreWithSealAndUINoCleanup(t, &CoreConfig{})
	testCoreUnsealed(t, c)
	doneCh := c.ShutdownDone()
	errs := make(chan error, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		err := c.Shutdown()
		errs <- err
	}()

	// wait for it
	err := <-errs
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-doneCh:
		if !c.Sealed() {
			t.Fatal("shutdown done called prematurely!")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown notification not received")
	}
}

// Attempt to seal bad token
func TestCore_Seal_BadToken(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)
	if err := c.Seal("foo"); err == nil {
		t.Fatalf("err: %v", err)
	}
	if c.Sealed() {
		t.Fatal("was sealed")
	}
}

func TestCore_OneTenPlus_BatchTokens(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)

	// load up some versions and ensure that 1.10 is the most recent version
	upgradeTimePlusEpsilon := time.Now().UTC()

	versionEntries := []VaultVersion{
		{Version: "1.9.2", TimestampInstalled: upgradeTimePlusEpsilon.Add(-4 * time.Hour)},
		{Version: "1.10.1", TimestampInstalled: upgradeTimePlusEpsilon.Add(2 * time.Hour)},
	}

	for _, entry := range versionEntries {
		_, err := c.storeVersionEntry(context.Background(), &entry, false)
		if err != nil {
			t.Fatalf("failed to write version entry %#v, err: %s", entry, err.Error())
		}
	}

	err := c.loadVersionHistory(c.activeContext)
	if err != nil {
		t.Fatalf("failed to populate version history cache, err: %s", err.Error())
	}

	// double check that we're working with 1.10
	v, _, err := c.FindNewestVersionTimestamp()
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.10.1" {
		t.Fatalf("expected 1.10.1, found: %s", v)
	}

	// generate a batch token
	te := &logical.TokenEntry{
		NumUses:     1,
		Policies:    []string{"root"},
		NamespaceID: namespace.RootNamespaceID,
		Type:        logical.TokenTypeBatch,
	}
	err = c.tokenStore.create(namespace.RootContext(nil), te, true)
	if err != nil {
		t.Fatal(err)
	}

	// verify it uses the legacy prefix
	if !strings.HasPrefix(te.ID, consts.BatchTokenPrefix) {
		t.Fatalf("expected 1.10 batch token IDs to start with hvb. but it didn't: %s", te.ID)
	}
}

// GH-3497
func TestCore_Seal_SingleUse(t *testing.T) {
	c, keys, _ := TestCoreUnsealed(t)
	c.tokenStore.create(namespace.RootContext(nil), &logical.TokenEntry{
		ID:          "foo",
		NumUses:     1,
		Policies:    []string{"root"},
		NamespaceID: namespace.RootNamespaceID,
	}, true)
	if err := c.Seal("foo"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !c.Sealed() {
		t.Fatal("not sealed")
	}
	for i, key := range keys {
		unseal, err := TestCoreUnseal(c, key)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if i+1 == len(keys) && !unseal {
			t.Fatal("err: should be unsealed")
		}
	}
	if err := c.Seal("foo"); err == nil {
		t.Fatal("expected error from revoked token")
	}
	te, err := c.tokenStore.Lookup(namespace.RootContext(nil), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if te != nil {
		t.Fatalf("expected nil token entry, got %#v", *te)
	}
}

// Ensure we get a LeaseID
func TestCore_HandleRequest_Lease(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: root,
	}
	ctx := namespace.RootContext(nil)
	resp, err := c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(ctx, req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Data == nil {
		t.Fatalf("bad: %#v", resp)
	}
	if resp.Secret.TTL != time.Hour {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Data["foo"] != "bar" {
		t.Fatalf("bad: %#v", resp.Data)
	}
}

func TestCore_HandleRequest_Lease_MaxLength(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1000h",
		},
		ClientToken: root,
	}
	ctx := namespace.RootContext(nil)
	resp, err := c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(ctx, req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Data == nil {
		t.Fatalf("bad: %#v", resp)
	}
	if resp.Secret.TTL != c.maxLeaseTTL {
		t.Fatalf("bad: %#v, %d", resp.Secret, c.maxLeaseTTL)
	}
	if resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Data["foo"] != "bar" {
		t.Fatalf("bad: %#v", resp.Data)
	}
}

func TestCore_HandleRequest_Lease_DefaultLength(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "0h",
		},
		ClientToken: root,
	}
	ctx := namespace.RootContext(nil)
	resp, err := c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(ctx, req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(ctx, req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Data == nil {
		t.Fatalf("bad: %#v", resp)
	}
	if resp.Secret.TTL != c.defaultLeaseTTL {
		t.Fatalf("bad: %d, %d", resp.Secret.TTL/time.Second, c.defaultLeaseTTL/time.Second)
	}
	if resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	if resp.Data["foo"] != "bar" {
		t.Fatalf("bad: %#v", resp.Data)
	}
}

func TestCore_HandleRequest_MissingToken(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err == nil || !errwrap.Contains(err, logical.ErrPermissionDenied.Error()) {
		t.Fatalf("err: %v", err)
	}
	if resp.Data["error"] != logical.ErrPermissionDenied.Error() {
		t.Fatalf("bad: %#v", resp)
	}
}

func TestCore_HandleRequest_InvalidToken(t *testing.T) {
	c, _, _ := TestCoreUnsealed(t)

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: "foobarbaz",
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err == nil || !errwrap.Contains(err, logical.ErrPermissionDenied.Error()) {
		t.Fatalf("err: %v", err)
	}
	if resp.Data["error"] != "permission denied" {
		t.Fatalf("bad: %#v", resp)
	}
}

// Check that standard permissions work
func TestCore_HandleRequest_NoSlash(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	req := &logical.Request{
		Operation:   logical.HelpOperation,
		Path:        "secret",
		ClientToken: root,
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v, resp: %v", err, resp)
	}
	if _, ok := resp.Data["help"]; !ok {
		t.Fatalf("resp: %v", resp)
	}
}

// Test a root path is denied if non-root
func TestCore_HandleRequest_RootPath(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)
	testMakeServiceTokenViaCore(t, c, root, "child", "", []string{"test"})

	req := &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "sys/policy", // root protected!
		ClientToken: "child",
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err == nil || !errwrap.Contains(err, logical.ErrPermissionDenied.Error()) {
		t.Fatalf("err: %v, resp: %v", err, resp)
	}
}

// Test a root path is allowed if non-root but with sudo
func TestCore_HandleRequest_RootPath_WithSudo(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	// Set the 'test' policy object to permit access to sys/policy
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "sys/policy/test", // root protected!
		Data: map[string]interface{}{
			"rules": `path "sys/policy" { policy = "sudo" }`,
		},
		ClientToken: root,
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil && (resp.IsError() || len(resp.Data) > 0) {
		t.Fatalf("bad: %#v", resp)
	}

	// Child token (non-root) but with 'test' policy should have access
	testMakeServiceTokenViaCore(t, c, root, "child", "", []string{"test"})
	req = &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "sys/policy", // root protected!
		ClientToken: "child",
	}
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil {
		t.Fatalf("bad: %#v", resp)
	}
}

// Check that standard permissions work
func TestCore_HandleRequest_PermissionDenied(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)
	testMakeServiceTokenViaCore(t, c, root, "child", "", []string{"test"})

	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: "child",
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err == nil || !errwrap.Contains(err, logical.ErrPermissionDenied.Error()) {
		t.Fatalf("err: %v, resp: %v", err, resp)
	}
}

// Check that standard permissions work
func TestCore_HandleRequest_PermissionAllowed(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)
	testMakeServiceTokenViaCore(t, c, root, "child", "", []string{"test"})

	// Set the 'test' policy object to permit access to secret/
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "sys/policy/test",
		Data: map[string]interface{}{
			"rules": `path "secret/*" { policy = "write" }`,
		},
		ClientToken: root,
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil && (resp.IsError() || len(resp.Data) > 0) {
		t.Fatalf("bad: %#v", resp)
	}

	// Write should work now
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: "child",
	}
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}
}

func TestCore_HandleRequest_NoClientToken(t *testing.T) {
	noop := &NoopBackend{
		Response: &logical.Response{},
	}
	c, _, root := TestCoreUnsealed(t)
	c.logicalBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the logical backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo")
	req.Data["type"] = "noop"
	req.Data["description"] = "foo"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to request with connection data
	req = &logical.Request{
		Path: "foo/login",
	}
	req.ClientToken = root
	if _, err := c.HandleRequest(namespace.RootContext(nil), req); err != nil {
		t.Fatalf("err: %v", err)
	}

	ct := noop.Requests[0].ClientToken
	if ct == "" || ct == root {
		t.Fatalf("bad: %#v", noop.Requests)
	}
}

func TestCore_HandleRequest_ConnOnLogin(t *testing.T) {
	noop := &NoopBackend{
		Login:       []string{"login"},
		Response:    &logical.Response{},
		BackendType: logical.TypeCredential,
	}
	c, _, root := TestCoreUnsealed(t)
	c.credentialBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/auth/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to request with connection data
	req = &logical.Request{
		Path:       "auth/foo/login",
		Connection: &logical.Connection{},
	}
	if _, err := c.HandleRequest(namespace.RootContext(nil), req); err != nil {
		t.Fatalf("err: %v", err)
	}
	if noop.Requests[0].Connection == nil {
		t.Fatalf("bad: %#v", noop.Requests)
	}
}

// Ensure we get a client token
func TestCore_HandleLogin_Token(t *testing.T) {
	noop := &NoopBackend{
		Login: []string{"login"},
		Response: &logical.Response{
			Auth: &logical.Auth{
				Policies: []string{"foo", "bar"},
				Metadata: map[string]string{
					"user": "armon",
				},
				DisplayName: "armon",
			},
		},
		BackendType: logical.TypeCredential,
	}
	c, _, root := TestCoreUnsealed(t)
	c.credentialBackends["noop"] = func(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/auth/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to login
	lreq := &logical.Request{
		Path: "auth/foo/login",
	}
	lresp, err := c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we got a client token back
	clientToken := lresp.Auth.ClientToken
	if clientToken == "" {
		t.Fatalf("bad: %#v", lresp)
	}

	// Check the policy and metadata
	innerToken, _ := c.DecodeSSCToken(clientToken)
	te, err := c.tokenStore.Lookup(namespace.RootContext(nil), innerToken)
	if err != nil || te == nil {
		t.Fatalf("tok: %s, err: %v", clientToken, err)
	}

	expectedID, _ := c.DecodeSSCToken(clientToken)
	expect := &logical.TokenEntry{
		ID:       expectedID,
		Accessor: te.Accessor,
		Parent:   "",
		Policies: []string{"bar", "default", "foo"},
		Path:     "auth/foo/login",
		Meta: map[string]string{
			"user": "armon",
		},
		DisplayName:  "foo-armon",
		TTL:          time.Hour * 24,
		CreationTime: te.CreationTime,
		NamespaceID:  namespace.RootNamespaceID,
		CubbyholeID:  te.CubbyholeID,
		Type:         logical.TokenTypeService,
	}

	if diff := deep.Equal(te, expect); diff != nil {
		t.Fatal(diff)
	}

	// Check that we have a lease with default duration
	if lresp.Auth.TTL != noop.System().DefaultLeaseTTL() {
		t.Fatalf("bad: %#v, defaultLeaseTTL: %#v", lresp.Auth, c.defaultLeaseTTL)
	}
}

func TestCore_HandleRequest_AuditTrail(t *testing.T) {
	// Create a noop audit backend
	noop := &corehelpers.NoopAudit{}
	c, _, root := TestCoreUnsealed(t)
	c.auditBackends["noop"] = func(ctx context.Context, config *audit.BackendConfig) (audit.Backend, error) {
		noop = &corehelpers.NoopAudit{
			Config: config,
		}
		return noop, nil
	}

	// Enable the audit backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/audit/noop")
	req.Data["type"] = "noop"
	req.ClientToken = root
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Make a request
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: root,
	}
	req.ClientToken = root
	if _, err := c.HandleRequest(namespace.RootContext(nil), req); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the audit trail on request and response
	if len(noop.ReqAuth) != 1 {
		t.Fatalf("bad: %#v", noop)
	}
	auth := noop.ReqAuth[0]
	if auth.ClientToken != root {
		t.Fatalf("bad client token: %#v", auth)
	}
	if len(auth.Policies) != 1 || auth.Policies[0] != "root" {
		t.Fatalf("bad: %#v", auth)
	}
	if len(noop.Req) != 1 || !reflect.DeepEqual(noop.Req[0], req) {
		t.Fatalf("Bad: %#v", noop.Req[0])
	}

	if len(noop.RespAuth) != 2 {
		t.Fatalf("bad: %#v", noop)
	}
	if !reflect.DeepEqual(noop.RespAuth[1], auth) {
		t.Fatalf("bad: %#v, vs %#v", auth, noop.RespAuth)
	}
	if len(noop.RespReq) != 2 || !reflect.DeepEqual(noop.RespReq[1], req) {
		t.Fatalf("Bad: %#v", noop.RespReq[1])
	}
	if len(noop.Resp) != 2 || !reflect.DeepEqual(noop.Resp[1], resp) {
		t.Fatalf("Bad: %#v", noop.Resp[1])
	}
}

func TestCore_HandleRequest_AuditTrail_noHMACKeys(t *testing.T) {
	// Create a noop audit backend
	var noop *corehelpers.NoopAudit
	c, _, root := TestCoreUnsealed(t)
	c.auditBackends["noop"] = func(ctx context.Context, config *audit.BackendConfig) (audit.Backend, error) {
		noop = &corehelpers.NoopAudit{
			Config: config,
		}
		return noop, nil
	}

	// Specify some keys to not HMAC
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/secret/tune")
	req.Data["audit_non_hmac_request_keys"] = "foo"
	req.ClientToken = root
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	req = logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/secret/tune")
	req.Data["audit_non_hmac_response_keys"] = "baz"
	req.ClientToken = root
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Enable the audit backend
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/audit/noop")
	req.Data["type"] = "noop"
	req.ClientToken = root
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Make a request
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo": "bar",
		},
		ClientToken: root,
	}
	req.ClientToken = root
	if _, err := c.HandleRequest(namespace.RootContext(nil), req); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the audit trail on request and response
	if len(noop.ReqAuth) != 1 {
		t.Fatalf("bad: %#v", noop)
	}
	auth := noop.ReqAuth[0]
	if auth.ClientToken != root {
		t.Fatalf("bad client token: %#v", auth)
	}
	if len(auth.Policies) != 1 || auth.Policies[0] != "root" {
		t.Fatalf("bad: %#v", auth)
	}
	if len(noop.Req) != 1 || !reflect.DeepEqual(noop.Req[0], req) {
		t.Fatalf("Bad: %#v", noop.Req[0])
	}
	if len(noop.ReqNonHMACKeys) != 1 || noop.ReqNonHMACKeys[0] != "foo" {
		t.Fatalf("Bad: %#v", noop.ReqNonHMACKeys)
	}
	if len(noop.RespAuth) != 2 {
		t.Fatalf("bad: %#v", noop)
	}
	if !reflect.DeepEqual(noop.RespAuth[1], auth) {
		t.Fatalf("bad: %#v", auth)
	}
	if len(noop.RespReq) != 2 || !reflect.DeepEqual(noop.RespReq[1], req) {
		t.Fatalf("Bad: %#v", noop.RespReq[1])
	}
	if len(noop.Resp) != 2 || !reflect.DeepEqual(noop.Resp[1], resp) {
		t.Fatalf("Bad: %#v", noop.Resp[1])
	}

	// Test for response keys
	// Make a request
	req = &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "secret/test",
		ClientToken: root,
	}
	req.ClientToken = root
	err = c.PopulateTokenEntry(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if _, err := c.HandleRequest(namespace.RootContext(nil), req); err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(noop.RespNonHMACKeys) != 1 || !strutil.EquivalentSlices(noop.RespNonHMACKeys[0], []string{"baz"}) {
		t.Fatalf("Bad: %#v", noop.RespNonHMACKeys)
	}
	if len(noop.RespReqNonHMACKeys) != 1 || !strutil.EquivalentSlices(noop.RespReqNonHMACKeys[0], []string{"foo"}) {
		t.Fatalf("Bad: %#v", noop.RespReqNonHMACKeys)
	}
}

func TestCore_HandleLogin_AuditTrail(t *testing.T) {
	// Create a badass credential backend that always logs in as armon
	noop := &corehelpers.NoopAudit{}
	noopBack := &NoopBackend{
		Login: []string{"login"},
		Response: &logical.Response{
			Auth: &logical.Auth{
				LeaseOptions: logical.LeaseOptions{
					TTL: time.Hour,
				},
				Policies: []string{"foo", "bar"},
				Metadata: map[string]string{
					"user": "armon",
				},
			},
		},
		BackendType: logical.TypeCredential,
	}
	c, _, root := TestCoreUnsealed(t)
	c.credentialBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noopBack, nil
	}
	c.auditBackends["noop"] = func(ctx context.Context, config *audit.BackendConfig) (audit.Backend, error) {
		noop = &corehelpers.NoopAudit{
			Config: config,
		}
		return noop, nil
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/auth/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Enable the audit backend
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/audit/noop")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to login
	lreq := &logical.Request{
		Path: "auth/foo/login",
	}
	lresp, err := c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we got a client token back
	clientToken := lresp.Auth.ClientToken
	if clientToken == "" {
		t.Fatalf("bad: %#v", lresp)
	}

	// Check the audit trail on request and response
	if len(noop.ReqAuth) != 1 {
		t.Fatalf("bad: %#v", noop)
	}
	if len(noop.Req) != 1 || !reflect.DeepEqual(noop.Req[0], lreq) {
		t.Fatalf("Bad: %#v %#v", noop.Req[0], lreq)
	}

	if len(noop.RespAuth) != 2 {
		t.Fatalf("bad: %#v", noop)
	}
	auth := noop.RespAuth[1]
	if auth.ClientToken != clientToken {
		t.Fatalf("bad client token: %#v", auth)
	}
	if len(auth.Policies) != 3 || auth.Policies[0] != "bar" || auth.Policies[1] != "default" || auth.Policies[2] != "foo" {
		t.Fatalf("bad: %#v", auth)
	}
	if len(noop.RespReq) != 2 || !reflect.DeepEqual(noop.RespReq[1], lreq) {
		t.Fatalf("Bad: %#v", noop.RespReq[1])
	}
	if len(noop.Resp) != 2 || !reflect.DeepEqual(noop.Resp[1], lresp) {
		t.Fatalf("Bad: %#v %#v", noop.Resp[1], lresp)
	}
}

// Check that we register a lease for new tokens
func TestCore_HandleRequest_CreateToken_Lease(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	// Create a new credential
	req := logical.TestRequest(t, logical.UpdateOperation, "auth/token/create")
	req.ClientToken = root
	req.Data["policies"] = []string{"foo"}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we got a new client token back
	if resp.IsError() {
		t.Fatalf("err: %v %v", err, *resp)
	}
	clientToken := resp.Auth.ClientToken
	if clientToken == "" {
		t.Fatalf("bad: %#v", resp)
	}

	// Check the policy and metadata
	te, err := c.tokenStore.Lookup(namespace.RootContext(nil), clientToken)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	expectedID, _ := c.DecodeSSCToken(clientToken)
	expectedRootID, _ := c.DecodeSSCToken(root)

	expect := &logical.TokenEntry{
		ID:           expectedID,
		Accessor:     te.Accessor,
		Parent:       expectedRootID,
		Policies:     []string{"default", "foo"},
		Path:         "auth/token/create",
		DisplayName:  "token",
		CreationTime: te.CreationTime,
		TTL:          time.Hour * 24 * 32,
		NamespaceID:  namespace.RootNamespaceID,
		CubbyholeID:  te.CubbyholeID,
		Type:         logical.TokenTypeService,
	}
	if diff := deep.Equal(te, expect); diff != nil {
		t.Fatal(diff)
	}

	// Check that we have a lease with default duration
	if resp.Auth.TTL != c.defaultLeaseTTL {
		t.Fatalf("bad: %#v", resp.Auth)
	}
}

// Check that we handle excluding the default policy
func TestCore_HandleRequest_CreateToken_NoDefaultPolicy(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	// Create a new credential
	req := logical.TestRequest(t, logical.UpdateOperation, "auth/token/create")
	req.ClientToken = root
	req.Data["policies"] = []string{"foo"}
	req.Data["no_default_policy"] = true
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we got a new client token back
	clientToken := resp.Auth.ClientToken
	if clientToken == "" {
		t.Fatalf("bad: %#v", resp)
	}

	// Check the policy and metadata
	te, err := c.tokenStore.Lookup(namespace.RootContext(nil), clientToken)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	expectedID, _ := c.DecodeSSCToken(clientToken)
	expectedRootID, _ := c.DecodeSSCToken(root)

	expect := &logical.TokenEntry{
		ID:           expectedID,
		Accessor:     te.Accessor,
		Parent:       expectedRootID,
		Policies:     []string{"foo"},
		Path:         "auth/token/create",
		DisplayName:  "token",
		CreationTime: te.CreationTime,
		TTL:          time.Hour * 24 * 32,
		NamespaceID:  namespace.RootNamespaceID,
		CubbyholeID:  te.CubbyholeID,
		Type:         logical.TokenTypeService,
	}
	if diff := deep.Equal(te, expect); diff != nil {
		t.Fatal(diff)
	}
}

func TestCore_LimitedUseToken(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	// Create a new credential
	req := logical.TestRequest(t, logical.UpdateOperation, "auth/token/create")
	req.ClientToken = root
	req.Data["num_uses"] = "1"
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Put a secret
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/foo",
		Data: map[string]interface{}{
			"foo": "bar",
		},
		ClientToken: resp.Auth.ClientToken,
	}
	_, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Second operation should fail
	_, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err == nil || !errwrap.Contains(err, logical.ErrPermissionDenied.Error()) {
		t.Fatalf("err: %v", err)
	}
}

func TestCore_Standby_Seal(t *testing.T) {
	// Create the first core and initialize it
	logger = logging.NewVaultLogger(log.Trace)

	inm, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	redirectOriginal := "http://127.0.0.1:8200"
	core, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core.Shutdown()
	keys, root := TestCoreInit(t, core)
	for _, key := range keys {
		if _, err := TestCoreUnseal(core, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Wait for core to become active
	TestWaitActive(t, core)

	// Check the leader is local
	isLeader, advertise, _, err := core.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Create the second core and initialize it
	redirectOriginal2 := "http://127.0.0.1:8500"
	core2, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal2,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core2.Shutdown()
	for _, key := range keys {
		if _, err := TestCoreUnseal(core2, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core2.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Core2 should be in standby
	standby, err := core2.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Check the leader is not local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if isLeader {
		t.Fatal("should not be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Seal the standby core with the correct token. Shouldn't go down
	err = core2.Seal(root)
	if err == nil {
		t.Fatal("should not be sealed")
	}

	keyUUID, err := uuid.GenerateUUID()
	if err != nil {
		t.Fatal(err)
	}
	// Seal the standby core with an invalid token. Shouldn't go down
	err = core2.Seal(keyUUID)
	if err == nil {
		t.Fatal("should not be sealed")
	}
}

func TestCore_StepDown(t *testing.T) {
	// Create the first core and initialize it
	logger = logging.NewVaultLogger(log.Trace).Named(t.Name())

	inm, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	redirectOriginal := "http://127.0.0.1:8200"
	core, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal,
		Logger:       logger.Named("core1"),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core.Shutdown()
	keys, root := TestCoreInit(t, core)
	for _, key := range keys {
		if _, err := TestCoreUnseal(core, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Wait for core to become active
	TestWaitActive(t, core)

	// Check the leader is local
	isLeader, advertise, _, err := core.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Create the second core and initialize it
	redirectOriginal2 := "http://127.0.0.1:8500"
	core2, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal2,
		Logger:       logger.Named("core2"),
	})
	defer core2.Shutdown()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, key := range keys {
		if _, err := TestCoreUnseal(core2, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core2.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Core2 should be in standby
	standby, err := core2.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Check the leader is not local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if isLeader {
		t.Fatal("should not be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	req := &logical.Request{
		ClientToken: root,
		Path:        "sys/step-down",
	}

	// Create an identifier for the request
	req.ID, err = uuid.GenerateUUID()
	if err != nil {
		t.Fatalf("failed to generate identifier for the request: path: %s err: %v", req.Path, err)
	}

	// Step down core
	err = core.StepDown(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatal("error stepping down core 1")
	}

	// Give time to switch leaders
	time.Sleep(5 * time.Second)

	// Core1 should be in standby
	standby, err = core.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Check the leader is core2
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal2 {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal2)
	}

	// Check the leader is not local
	isLeader, advertise, _, err = core.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if isLeader {
		t.Fatal("should not be leader")
	}
	if advertise != redirectOriginal2 {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal2)
	}

	// Step down core2
	err = core2.StepDown(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatal("error stepping down core 1")
	}

	// Give time to switch leaders -- core 1 will still be waiting on its
	// cooling off period so give it a full 10 seconds to recover
	time.Sleep(10 * time.Second)

	// Core2 should be in standby
	standby, err = core2.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Check the leader is core1
	isLeader, advertise, _, err = core.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Check the leader is not local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if isLeader {
		t.Fatal("should not be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}
}

func TestCore_CleanLeaderPrefix(t *testing.T) {
	// Create the first core and initialize it
	logger = logging.NewVaultLogger(log.Trace)

	inm, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	redirectOriginal := "http://127.0.0.1:8200"
	core, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core.Shutdown()
	keys, root := TestCoreInit(t, core)
	for _, key := range keys {
		if _, err := TestCoreUnseal(core, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Wait for core to become active
	TestWaitActive(t, core)

	// Ensure that the original clean function has stopped running
	time.Sleep(2 * time.Second)

	// Put several random entries
	for i := 0; i < 5; i++ {
		keyUUID, err := uuid.GenerateUUID()
		if err != nil {
			t.Fatal(err)
		}
		valueUUID, err := uuid.GenerateUUID()
		if err != nil {
			t.Fatal(err)
		}
		core.barrier.Put(namespace.RootContext(nil), &logical.StorageEntry{
			Key:   coreLeaderPrefix + keyUUID,
			Value: []byte(valueUUID),
		})
	}

	entries, err := core.barrier.List(namespace.RootContext(nil), coreLeaderPrefix)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(entries) != 6 {
		t.Fatalf("wrong number of core leader prefix entries, got %d", len(entries))
	}

	// Check the leader is local
	isLeader, advertise, _, err := core.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Create a second core, attached to same in-memory store
	redirectOriginal2 := "http://127.0.0.1:8500"
	core2, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal2,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core2.Shutdown()
	for _, key := range keys {
		if _, err := TestCoreUnseal(core2, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core2.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Core2 should be in standby
	standby, err := core2.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Check the leader is not local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if isLeader {
		t.Fatal("should not be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Seal the first core, should step down
	err = core.Seal(root)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Core should be in standby
	standby, err = core.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Wait for core2 to become active
	TestWaitActive(t, core2)

	// Check the leader is local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal2 {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal2)
	}

	// Give time for the entries to clear out; it is conservative at 1/second
	time.Sleep(10 * leaderPrefixCleanDelay)

	entries, err = core2.barrier.List(namespace.RootContext(nil), coreLeaderPrefix)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("wrong number of core leader prefix entries, got %d", len(entries))
	}
}

func TestCore_Standby(t *testing.T) {
	logger = logging.NewVaultLogger(log.Trace)

	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	testCore_Standby_Common(t, inmha, inmha.(physical.HABackend))
}

func TestCore_Standby_SeparateHA(t *testing.T) {
	logger = logging.NewVaultLogger(log.Trace)

	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	inmha2, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	testCore_Standby_Common(t, inmha, inmha2.(physical.HABackend))
}

func testCore_Standby_Common(t *testing.T, inm physical.Backend, inmha physical.HABackend) {
	// Create the first core and initialize it
	redirectOriginal := "http://127.0.0.1:8200"
	core, err := NewCore(&CoreConfig{
		Physical:        inm,
		HAPhysical:      inmha,
		RedirectAddr:    redirectOriginal,
		BuiltinRegistry: corehelpers.NewMockBuiltinRegistry(),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core.Shutdown()
	keys, root := TestCoreInit(t, core)
	for _, key := range keys {
		if _, err := TestCoreUnseal(core, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Wait for core to become active
	TestWaitActive(t, core)

	testCoreAddSecretMount(t, core, root)

	// Put a secret
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/foo",
		Data: map[string]interface{}{
			"foo": "bar",
		},
		ClientToken: root,
	}
	_, err = core.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the leader is local
	isLeader, advertise, _, err := core.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Create a second core, attached to same in-memory store
	redirectOriginal2 := "http://127.0.0.1:8500"
	core2, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha,
		RedirectAddr: redirectOriginal2,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core2.Shutdown()
	for _, key := range keys {
		if _, err := TestCoreUnseal(core2, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Verify unsealed
	if core2.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Core2 should be in standby
	standby, err := core2.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Request should fail in standby mode
	_, err = core2.HandleRequest(namespace.RootContext(nil), req)
	if err != consts.ErrStandby {
		t.Fatalf("err: %v", err)
	}

	// Check the leader is not local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if isLeader {
		t.Fatal("should not be leader")
	}
	if advertise != redirectOriginal {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal)
	}

	// Seal the first core, should step down
	err = core.Seal(root)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Core should be in standby
	standby, err = core.Standby()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !standby {
		t.Fatal("should be standby")
	}

	// Wait for core2 to become active
	TestWaitActive(t, core2)

	// Read the secret
	req = &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "secret/foo",
		ClientToken: root,
	}
	resp, err := core2.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify the response
	if resp.Data["foo"] != "bar" {
		t.Fatalf("bad: %#v", resp)
	}

	// Check the leader is local
	isLeader, advertise, _, err = core2.Leader()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !isLeader {
		t.Fatal("should be leader")
	}
	if advertise != redirectOriginal2 {
		t.Fatalf("Bad advertise: %v, orig is %v", advertise, redirectOriginal2)
	}

	if inm.(*inmem.InmemHABackend) == inmha.(*inmem.InmemHABackend) {
		lockSize := inm.(*inmem.InmemHABackend).LockMapSize()
		if lockSize == 0 {
			t.Fatal("locks not used with only one HA backend")
		}
	} else {
		lockSize := inmha.(*inmem.InmemHABackend).LockMapSize()
		if lockSize == 0 {
			t.Fatal("locks not used with expected HA backend")
		}

		lockSize = inm.(*inmem.InmemHABackend).LockMapSize()
		if lockSize != 0 {
			t.Fatal("locks used with unexpected HA backend")
		}
	}
}

// Ensure that InternalData is never returned
func TestCore_HandleRequest_Login_InternalData(t *testing.T) {
	noop := &NoopBackend{
		Login: []string{"login"},
		Response: &logical.Response{
			Auth: &logical.Auth{
				Policies: []string{"foo", "bar"},
				InternalData: map[string]interface{}{
					"foo": "bar",
				},
			},
		},
		BackendType: logical.TypeCredential,
	}

	c, _, root := TestCoreUnsealed(t)
	c.credentialBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/auth/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to login
	lreq := &logical.Request{
		Path: "auth/foo/login",
	}
	lresp, err := c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we do not get the internal data
	if lresp.Auth.InternalData != nil {
		t.Fatalf("bad: %#v", lresp)
	}
}

// Ensure that InternalData is never returned
func TestCore_HandleRequest_InternalData(t *testing.T) {
	noop := &NoopBackend{
		Response: &logical.Response{
			Secret: &logical.Secret{
				InternalData: map[string]interface{}{
					"foo": "bar",
				},
			},
			Data: map[string]interface{}{
				"foo": "bar",
			},
		},
	}

	c, _, root := TestCoreUnsealed(t)
	c.logicalBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to read
	lreq := &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "foo/test",
		ClientToken: root,
	}
	lreq.SetTokenEntry(&logical.TokenEntry{ID: root, NamespaceID: "root", Policies: []string{"root"}})
	lresp, err := c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we do not get the internal data
	if lresp.Secret.InternalData != nil {
		t.Fatalf("bad: %#v", lresp)
	}
}

// Ensure login does not return a secret
func TestCore_HandleLogin_ReturnSecret(t *testing.T) {
	// Create a badass credential backend that always logs in as armon
	noopBack := &NoopBackend{
		Login: []string{"login"},
		Response: &logical.Response{
			Secret: &logical.Secret{},
			Auth: &logical.Auth{
				Policies: []string{"foo", "bar"},
			},
		},
		BackendType: logical.TypeCredential,
	}
	c, _, root := TestCoreUnsealed(t)
	c.credentialBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noopBack, nil
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/auth/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to login
	lreq := &logical.Request{
		Path: "auth/foo/login",
	}
	_, err = c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != ErrInternalError {
		t.Fatalf("err: %v", err)
	}
}

// Renew should return the same lease back
func TestCore_RenewSameLease(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	// Create a leasable secret
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: root,
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}
	original := resp.Secret.LeaseID

	// Renew the lease
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/renew/"+resp.Secret.LeaseID)
	req.ClientToken = root
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify the lease did not change
	if resp.Secret.LeaseID != original {
		t.Fatalf("lease id changed: %s %s", original, resp.Secret.LeaseID)
	}

	// Renew the lease (alternate path)
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/leases/renew/"+resp.Secret.LeaseID)
	req.ClientToken = root
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify the lease did not change
	if resp.Secret.LeaseID != original {
		t.Fatalf("lease id changed: %s %s", original, resp.Secret.LeaseID)
	}
}

// Renew of a token should not create a new lease
func TestCore_RenewToken_SingleRegister(t *testing.T) {
	c, _, root := TestCoreUnsealed(t)

	// Create a new token
	req := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "auth/token/create",
		Data: map[string]interface{}{
			"lease": "1h",
		},
		ClientToken: root,
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	newClient := resp.Auth.ClientToken

	// Renew the token
	req = logical.TestRequest(t, logical.UpdateOperation, "auth/token/renew")
	req.ClientToken = newClient
	req.Data = map[string]interface{}{
		"token": newClient,
	}
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Revoke using the renew prefix
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/revoke-prefix/auth/token/renew/")
	req.ClientToken = root
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify our token is still valid (e.g. we did not get invalidated by the revoke)
	req = logical.TestRequest(t, logical.UpdateOperation, "auth/token/lookup")
	req.Data = map[string]interface{}{
		"token": newClient,
	}
	req.ClientToken = newClient
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify the token exists
	if newClient != resp.Data["id"].(string) {
		t.Fatalf("bad: return IDs: expected %v, got %v",
			resp.Data["id"], newClient)
	}
}

// Based on bug GH-203, attempt to disable a credential backend with leased secrets
func TestCore_EnableDisableCred_WithLease(t *testing.T) {
	noopBack := &NoopBackend{
		Login: []string{"login"},
		Response: &logical.Response{
			Auth: &logical.Auth{
				Policies: []string{"root"},
			},
		},
		BackendType: logical.TypeCredential,
	}

	c, _, root := TestCoreUnsealed(t)
	c.credentialBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noopBack, nil
	}

	secretWritingPolicy := `
name = "admins"
path "secret/*" {
	capabilities = ["update", "create", "read"]
}
`

	ps := c.policyStore
	policy, _ := ParseACLPolicy(namespace.RootNamespace, secretWritingPolicy)
	if err := ps.SetPolicy(namespace.RootContext(nil), policy, nil); err != nil {
		t.Fatal(err)
	}

	// Enable the credential backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/auth/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to login -- should fail because we don't allow root to be returned
	lreq := &logical.Request{
		Path: "auth/foo/login",
	}
	lresp, err := c.HandleRequest(namespace.RootContext(nil), lreq)
	if err == nil || lresp == nil || !lresp.IsError() {
		t.Fatal("expected error trying to auth and receive root policy")
	}

	// Fix and try again
	noopBack.Response.Auth.Policies = []string{"admins"}
	lreq = &logical.Request{
		Path: "auth/foo/login",
	}
	lresp, err = c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Create a leasable secret
	req = &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "secret/test",
		Data: map[string]interface{}{
			"foo":   "bar",
			"lease": "1h",
		},
		ClientToken: lresp.Auth.ClientToken,
	}
	resp, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp != nil {
		t.Fatalf("bad: %#v", resp)
	}

	// Read the key
	req.Operation = logical.ReadOperation
	req.Data = nil
	err = c.PopulateTokenEntry(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if resp == nil || resp.Secret == nil || resp.Secret.LeaseID == "" {
		t.Fatalf("bad: %#v", resp.Secret)
	}

	// Renew the lease
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/leases/renew")
	req.Data = map[string]interface{}{
		"lease_id": resp.Secret.LeaseID,
	}
	req.ClientToken = lresp.Auth.ClientToken
	_, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Disable the credential backend
	req = logical.TestRequest(t, logical.DeleteOperation, "sys/auth/foo")
	req.ClientToken = root
	resp, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v %#v", err, resp)
	}
}

func TestCore_HandleRequest_MountPointType(t *testing.T) {
	noop := &NoopBackend{
		Response: &logical.Response{},
	}
	c, _, root := TestCoreUnsealed(t)
	c.logicalBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the logical backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo")
	req.Data["type"] = "noop"
	req.Data["description"] = "foo"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to request
	req = &logical.Request{
		Operation:  logical.ReadOperation,
		Path:       "foo/test",
		Connection: &logical.Connection{},
	}
	req.ClientToken = root
	if _, err := c.HandleRequest(namespace.RootContext(nil), req); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify Path, MountPoint, and MountType
	if noop.Requests[0].Path != "test" {
		t.Fatalf("bad: %#v", noop.Requests)
	}
	if noop.Requests[0].MountPoint != "foo/" {
		t.Fatalf("bad: %#v", noop.Requests)
	}
	if noop.Requests[0].MountType != "noop" {
		t.Fatalf("bad: %#v", noop.Requests)
	}
}

func TestCore_Standby_Rotate(t *testing.T) {
	// Create the first core and initialize it
	logger = logging.NewVaultLogger(log.Trace)

	inm, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}

	redirectOriginal := "http://127.0.0.1:8200"
	core, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core.Shutdown()
	keys, root := TestCoreInit(t, core)
	for _, key := range keys {
		if _, err := TestCoreUnseal(core, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Wait for core to become active
	TestWaitActive(t, core)

	// Create a second core, attached to same in-memory store
	redirectOriginal2 := "http://127.0.0.1:8500"
	core2, err := NewCore(&CoreConfig{
		Physical:     inm,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: redirectOriginal2,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core2.Shutdown()
	for _, key := range keys {
		if _, err := TestCoreUnseal(core2, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Rotate the encryption key
	req := &logical.Request{
		Operation:   logical.UpdateOperation,
		Path:        "sys/rotate",
		ClientToken: root,
	}
	_, err = core.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Seal the first core, should step down
	err = core.Seal(root)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Wait for core2 to become active
	TestWaitActive(t, core2)

	// Read the key status
	req = &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "sys/key-status",
		ClientToken: root,
	}
	resp, err := core2.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Verify the response
	if resp.Data["term"] != 2 {
		t.Fatalf("bad: %#v", resp)
	}
}

func TestCore_HandleRequest_Headers(t *testing.T) {
	noop := &NoopBackend{
		Response: &logical.Response{
			Data: map[string]interface{}{},
		},
	}

	c, _, root := TestCoreUnsealed(t)
	c.logicalBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Mount tune
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo/tune")
	req.Data["passthrough_request_headers"] = []string{"Should-Passthrough", "should-passthrough-case-insensitive"}
	req.ClientToken = root
	_, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to read
	lreq := &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "foo/test",
		ClientToken: root,
		Headers: map[string][]string{
			"Should-Passthrough":                  {"foo"},
			"Should-Passthrough-Case-Insensitive": {"baz"},
			"Should-Not-Passthrough":              {"bar"},
			consts.AuthHeaderName:                 {"nope"},
		},
	}
	_, err = c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the headers
	headers := noop.Requests[0].Headers

	// Test passthrough values
	if val, ok := headers["Should-Passthrough"]; ok {
		expected := []string{"foo"}
		if !reflect.DeepEqual(val, expected) {
			t.Fatalf("expected: %v, got: %v", expected, val)
		}
	} else {
		t.Fatal("expected 'Should-Passthrough' to be present in the headers map")
	}

	if val, ok := headers["Should-Passthrough-Case-Insensitive"]; ok {
		expected := []string{"baz"}
		if !reflect.DeepEqual(val, expected) {
			t.Fatalf("expected: %v, got: %v", expected, val)
		}
	} else {
		t.Fatal("expected 'Should-Passthrough-Case-Insensitive' to be present in the headers map")
	}

	if _, ok := headers["Should-Not-Passthrough"]; ok {
		t.Fatal("did not expect 'Should-Not-Passthrough' to be in the headers map")
	}

	if _, ok := headers[consts.AuthHeaderName]; ok {
		t.Fatalf("did not expect %q to be in the headers map", consts.AuthHeaderName)
	}
}

func TestCore_HandleRequest_Headers_denyList(t *testing.T) {
	noop := &NoopBackend{
		Response: &logical.Response{
			Data: map[string]interface{}{},
		},
	}

	c, _, root := TestCoreUnsealed(t)
	c.logicalBackends["noop"] = func(context.Context, *logical.BackendConfig) (logical.Backend, error) {
		return noop, nil
	}

	// Enable the backend
	req := logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo")
	req.Data["type"] = "noop"
	req.ClientToken = root
	_, err := c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Mount tune
	req = logical.TestRequest(t, logical.UpdateOperation, "sys/mounts/foo/tune")
	req.Data["passthrough_request_headers"] = []string{"Authorization", consts.AuthHeaderName}
	req.ClientToken = root
	_, err = c.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Attempt to read
	lreq := &logical.Request{
		Operation:   logical.ReadOperation,
		Path:        "foo/test",
		ClientToken: root,
		Headers: map[string][]string{
			consts.AuthHeaderName: {"foo"},
		},
	}
	_, err = c.HandleRequest(namespace.RootContext(nil), lreq)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Check the headers
	headers := noop.Requests[0].Headers

	// Test passthrough values, they should not be present in the backend
	if _, ok := headers[consts.AuthHeaderName]; ok {
		t.Fatalf("did not expect %q to be in the headers map", consts.AuthHeaderName)
	}
}

func TestCore_HandleRequest_TokenCreate_RegisterAuthFailure(t *testing.T) {
	core, _, root := TestCoreUnsealed(t)

	// Create a root token and use that for subsequent requests
	req := logical.TestRequest(t, logical.CreateOperation, "auth/token/create")
	req.Data = map[string]interface{}{
		"policies": []string{"root"},
	}
	req.ClientToken = root
	resp, err := core.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Auth == nil || resp.Auth.ClientToken == "" {
		t.Fatalf("expected a response from token creation, got: %#v", resp)
	}
	tokenWithRootPolicy := resp.Auth.ClientToken

	// Use new token to create yet a new token, this should succeed
	req = logical.TestRequest(t, logical.CreateOperation, "auth/token/create")
	req.ClientToken = tokenWithRootPolicy
	_, err = core.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatal(err)
	}

	// Try again but force failure on RegisterAuth to simulate a network failure
	// when registering the lease (e.g. a storage failure). This should trigger
	// an expiration manager cleanup on the newly created token
	core.expiration.testRegisterAuthFailure.Store(true)
	req = logical.TestRequest(t, logical.CreateOperation, "auth/token/create")
	req.ClientToken = tokenWithRootPolicy
	resp, err = core.HandleRequest(namespace.RootContext(nil), req)
	if err == nil {
		t.Fatalf("expected error, got a response: %#v", resp)
	}
	core.expiration.testRegisterAuthFailure.Store(false)

	// Do a lookup against the client token that we used for the failed request.
	// It should still be present
	req = logical.TestRequest(t, logical.UpdateOperation, "auth/token/lookup")
	req.Data = map[string]interface{}{
		"token": tokenWithRootPolicy,
	}
	req.ClientToken = root
	_, err = core.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatal(err)
	}

	// Do a token creation request with the token to ensure that it's still
	// valid, should succeed.
	req = logical.TestRequest(t, logical.CreateOperation, "auth/token/create")
	req.ClientToken = tokenWithRootPolicy
	resp, err = core.HandleRequest(namespace.RootContext(nil), req)
	if err != nil {
		t.Fatal(err)
	}
}

// mockServiceRegistration helps test whether standalone ServiceRegistration works
type mockServiceRegistration struct {
	notifyActiveCount int
	notifySealedCount int
	notifyPerfCount   int
	notifyInitCount   int
	runDiscoveryCount int
}

func (m *mockServiceRegistration) Run(shutdownCh <-chan struct{}, wait *sync.WaitGroup, redirectAddr string) error {
	m.runDiscoveryCount++
	return nil
}

func (m *mockServiceRegistration) NotifyActiveStateChange(isActive bool) error {
	m.notifyActiveCount++
	return nil
}

func (m *mockServiceRegistration) NotifySealedStateChange(isSealed bool) error {
	m.notifySealedCount++
	return nil
}

func (m *mockServiceRegistration) NotifyPerformanceStandbyStateChange(isStandby bool) error {
	m.notifyPerfCount++
	return nil
}

func (m *mockServiceRegistration) NotifyInitializedStateChange(isInitialized bool) error {
	m.notifyInitCount++
	return nil
}

// TestCore_ServiceRegistration tests whether standalone ServiceRegistration works
func TestCore_ServiceRegistration(t *testing.T) {
	// Make a mock service discovery
	sr := &mockServiceRegistration{}

	// Create the core
	logger = logging.NewVaultLogger(log.Trace)
	inm, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	const redirectAddr = "http://127.0.0.1:8200"
	core, err := NewCore(&CoreConfig{
		ServiceRegistration: sr,
		Physical:            inm,
		HAPhysical:          inmha.(physical.HABackend),
		RedirectAddr:        redirectAddr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Shutdown()

	// Vault should not yet be registered
	if diff := deep.Equal(sr, &mockServiceRegistration{}); diff != nil {
		t.Fatal(diff)
	}

	// Vault should be registered
	if diff := deep.Equal(sr, &mockServiceRegistration{
		runDiscoveryCount: 1,
	}); diff != nil {
		t.Fatal(diff)
	}

	// Initialize and unseal the core
	keys, _ := TestCoreInit(t, core)
	for _, key := range keys {
		if _, err := TestCoreUnseal(core, TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}
	if core.Sealed() {
		t.Fatal("should not be sealed")
	}

	// Wait for core to become active
	TestWaitActive(t, core)

	// Vault should be registered, unsealed, and active
	if diff := deep.Equal(sr, &mockServiceRegistration{
		runDiscoveryCount: 1,
		notifyActiveCount: 1,
		notifySealedCount: 1,
		notifyInitCount:   1,
	}); diff != nil {
		t.Fatal(diff)
	}
}

func TestExpiration_DeadlockDetection(t *testing.T) {
	testCore := TestCore(t)
	testCoreUnsealed(t, testCore)

	if testCore.expiration.DetectDeadlocks() {
		t.Fatal("expiration has deadlock detection enabled, it shouldn't")
	}

	testCore = TestCoreWithDeadlockDetection(t, nil, false)
	testCoreUnsealed(t, testCore)

	if !testCore.expiration.DetectDeadlocks() {
		t.Fatal("expiration doesn't have deadlock detection enabled, it should")
	}
}

func TestQuotas_DeadlockDetection(t *testing.T) {
	testCore := TestCore(t)
	testCoreUnsealed(t, testCore)

	if testCore.quotaManager.DetectDeadlocks() {
		t.Fatal("quotas has deadlock detection enabled, it shouldn't")
	}

	testCore = TestCoreWithDeadlockDetection(t, nil, false)
	testCoreUnsealed(t, testCore)

	if !testCore.quotaManager.DetectDeadlocks() {
		t.Fatal("quotas doesn't have deadlock detection enabled, it should")
	}
}

func TestStatelock_DeadlockDetection(t *testing.T) {
	testCore := TestCore(t)
	testCoreUnsealed(t, testCore)

	if testCore.DetectStateLockDeadlocks() {
		t.Fatal("statelock has deadlock detection enabled, it shouldn't")
	}

	testCore = TestCoreWithDeadlockDetection(t, nil, false)
	testCoreUnsealed(t, testCore)

	if !testCore.DetectStateLockDeadlocks() {
		t.Fatal("statelock doesn't have deadlock detection enabled, it should")
	}
}
