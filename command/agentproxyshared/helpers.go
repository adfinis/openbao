// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package agentproxyshared

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	log "github.com/hashicorp/go-hclog"
	"github.com/openbao/openbao/command/agentproxyshared/auth"
	"github.com/openbao/openbao/command/agentproxyshared/auth/approle"
	"github.com/openbao/openbao/command/agentproxyshared/auth/cert"
	"github.com/openbao/openbao/command/agentproxyshared/auth/jwt"
	"github.com/openbao/openbao/command/agentproxyshared/auth/kerberos"
	"github.com/openbao/openbao/command/agentproxyshared/auth/kubernetes"
	token_file "github.com/openbao/openbao/command/agentproxyshared/auth/token-file"
	"github.com/openbao/openbao/command/agentproxyshared/cache"
	"github.com/openbao/openbao/command/agentproxyshared/cache/cacheboltdb"
	"github.com/openbao/openbao/command/agentproxyshared/cache/cachememdb"
	"github.com/openbao/openbao/command/agentproxyshared/cache/keymanager"
)

// GetAutoAuthMethodFromConfig Calls the appropriate NewAutoAuthMethod function, initializing
// the auto-auth method, based on the auto-auth method type. Returns an error if one happens or
// the method type is invalid.
func GetAutoAuthMethodFromConfig(autoAuthMethodType string, authConfig *auth.AuthConfig, vaultAddress string) (auth.AuthMethod, error) {
	switch autoAuthMethodType {
	case "cert":
		return cert.NewCertAuthMethod(authConfig)
	case "jwt":
		return jwt.NewJWTAuthMethod(authConfig)
	case "kerberos":
		return kerberos.NewKerberosAuthMethod(authConfig)
	case "kubernetes":
		return kubernetes.NewKubernetesAuthMethod(authConfig)
	case "approle":
		return approle.NewApproleAuthMethod(authConfig)
	case "token_file":
		return token_file.NewTokenFileAuthMethod(authConfig)
	default:
		return nil, fmt.Errorf("unknown auth method %q", autoAuthMethodType)
	}
}

// PersistConfig contains configuration needed for persistent caching
type PersistConfig struct {
	Type                    string
	Path                    string `hcl:"path"`
	KeepAfterImport         bool   `hcl:"keep_after_import"`
	ExitOnErr               bool   `hcl:"exit_on_err"`
	ServiceAccountTokenFile string `hcl:"service_account_token_file"`
}

// AddPersistentStorageToLeaseCache adds persistence to a lease cache, based on a given PersistConfig
// Returns a close function to be deferred and the old token, if found, or an error
func AddPersistentStorageToLeaseCache(ctx context.Context, leaseCache *cache.LeaseCache, persistConfig *PersistConfig, logger log.Logger) (func() error, string, error) {
	if persistConfig == nil {
		return nil, "", errors.New("persist config was nil")
	}

	if persistConfig.Path == "" {
		return nil, "", errors.New("must specify persistent cache path")
	}

	// Set AAD based on key protection type
	var aad string
	var err error
	switch persistConfig.Type {
	case "kubernetes":
		aad, err = getServiceAccountJWT(persistConfig.ServiceAccountTokenFile)
		if err != nil {
			tokenFileName := persistConfig.ServiceAccountTokenFile
			if len(tokenFileName) == 0 {
				tokenFileName = "/var/run/secrets/kubernetes.io/serviceaccount/token"
			}
			return nil, "", fmt.Errorf("failed to read service account token from %s: %w", tokenFileName, err)
		}
	default:
		return nil, "", fmt.Errorf("persistent key protection type %q not supported", persistConfig.Type)
	}

	// Check if bolt file exists already
	dbFileExists, err := cacheboltdb.DBFileExists(persistConfig.Path)
	if err != nil {
		return nil, "", fmt.Errorf("failed to check if bolt file exists at path %s: %w", persistConfig.Path, err)
	}
	if dbFileExists {
		// Open the bolt file, but wait to setup Encryption
		ps, err := cacheboltdb.NewBoltStorage(&cacheboltdb.BoltStorageConfig{
			Path:   persistConfig.Path,
			Logger: logger.Named("cacheboltdb"),
		})
		if err != nil {
			return nil, "", fmt.Errorf("error opening persistent cache %v", err)
		}

		// Get the token from bolt for retrieving the encryption key,
		// then setup encryption so that restore is possible
		token, err := ps.GetRetrievalToken()
		if err != nil {
			return nil, "", fmt.Errorf("error getting retrieval token from persistent cache: %w", err)
		}

		if err := ps.Close(); err != nil {
			return nil, "", fmt.Errorf("failed to close persistent cache file after getting retrieval token: %w", err)
		}

		km, err := keymanager.NewPassthroughKeyManager(ctx, token)
		if err != nil {
			return nil, "", fmt.Errorf("failed to configure persistence encryption for cache: %w", err)
		}

		// Open the bolt file with the wrapper provided
		ps, err = cacheboltdb.NewBoltStorage(&cacheboltdb.BoltStorageConfig{
			Path:    persistConfig.Path,
			Logger:  logger.Named("cacheboltdb"),
			Wrapper: km.Wrapper(),
			AAD:     aad,
		})
		if err != nil {
			return nil, "", fmt.Errorf("error opening persistent cache with wrapper: %w", err)
		}

		// Restore anything in the persistent cache to the memory cache
		if err := leaseCache.Restore(ctx, ps); err != nil {
			logger.Error(fmt.Sprintf("error restoring in-memory cache from persisted file: %v", err))
			if persistConfig.ExitOnErr {
				return nil, "", errors.New("exiting with error as exit_on_err is set to true")
			}
		}
		logger.Info("loaded memcache from persistent storage")

		// Check for previous auto-auth token
		oldTokenBytes, err := ps.GetAutoAuthToken(ctx)
		if err != nil {
			logger.Error(fmt.Sprintf("error in fetching previous auto-auth token: %v", err))
			if persistConfig.ExitOnErr {
				return nil, "", errors.New("exiting with error as exit_on_err is set to true")
			}
		}
		var previousToken string
		if len(oldTokenBytes) > 0 {
			oldToken, err := cachememdb.Deserialize(oldTokenBytes)
			if err != nil {
				logger.Error(fmt.Sprintf("error in deserializing previous auto-auth token cache entryn: %v", err))
				if persistConfig.ExitOnErr {
					return nil, "", errors.New("exiting with error as exit_on_err is set to true")
				}
			}
			previousToken = oldToken.Token
		}

		// If keep_after_import true, set persistent storage layer in
		// leaseCache, else remove db file
		if persistConfig.KeepAfterImport {
			leaseCache.SetPersistentStorage(ps)
			return ps.Close, previousToken, nil
		} else {
			if err := ps.Close(); err != nil {
				logger.Warn(fmt.Sprintf("failed to close persistent cache file: %s", err))
			}
			dbFile := filepath.Join(persistConfig.Path, cacheboltdb.DatabaseFileName)
			if err := os.Remove(dbFile); err != nil {
				logger.Error(fmt.Sprintf("failed to remove persistent storage file %s: %v", dbFile, err))
				if persistConfig.ExitOnErr {
					return nil, "", errors.New("exiting with error as exit_on_err is set to true")
				}
			}
			return nil, previousToken, nil
		}
	} else {
		km, err := keymanager.NewPassthroughKeyManager(ctx, nil)
		if err != nil {
			return nil, "", fmt.Errorf("failed to configure persistence encryption for cache: %w", err)
		}
		ps, err := cacheboltdb.NewBoltStorage(&cacheboltdb.BoltStorageConfig{
			Path:    persistConfig.Path,
			Logger:  logger.Named("cacheboltdb"),
			Wrapper: km.Wrapper(),
			AAD:     aad,
		})
		if err != nil {
			return nil, "", fmt.Errorf("error creating persistent cache: %w", err)
		}
		logger.Info("configured persistent storage", "path", persistConfig.Path)

		// Stash the key material in bolt
		token, err := km.RetrievalToken(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("error getting persistence key: %w", err)
		}
		if err := ps.StoreRetrievalToken(token); err != nil {
			return nil, "", fmt.Errorf("error setting key in persistent cache: %w", err)
		}

		leaseCache.SetPersistentStorage(ps)
		return ps.Close, "", nil
	}
}

// getServiceAccountJWT attempts to read the service account JWT from the specified token file path.
// Defaults to using the Kubernetes default service account file path if token file path is empty.
func getServiceAccountJWT(tokenFile string) (string, error) {
	if len(tokenFile) == 0 {
		tokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	token, err := os.ReadFile(tokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(token)), nil
}
