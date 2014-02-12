// Copyright 2013 Joyent Inc.
// Licensed under the AGPLv3, see LICENCE file for details.

package joyent

import (
	"sync"

	"launchpad.net/gojoyent/jpc"

	"launchpad.net/juju-core/constraints"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/config"
	"launchpad.net/juju-core/environs/simplestreams"
	"launchpad.net/juju-core/environs/storage"
	"launchpad.net/juju-core/provider/common"
	"launchpad.net/juju-core/state"
	"launchpad.net/juju-core/state/api"
)

// This file contains the core of the Joyent Environ implementation.

type JoyentEnviron struct {
	name string
	// All mutating operations should lock the mutex. Non-mutating operations
	// should read all fields (other than name, which is immutable) from a
	// shallow copy taken with getSnapshot().
	// This advice is predicated on the goroutine-safety of the values of the
	// affected fields.
	lock    sync.Mutex
	ecfg    *environConfig
	creds   *jpc.Credentials
	storage storage.Storage
	compute *joyentCompute
}

var _ environs.Environ = (*JoyentEnviron)(nil)

func NewEnviron(cfg *config.Config) (*JoyentEnviron, error) {
	env := new(JoyentEnviron)
	err := env.SetConfig(cfg)
	if err != nil {
		return nil, err
	}
	env.name = cfg.Name()
	env.creds = getCredentials(env)
	env.storage = NewStorage(env, "")
	env.compute = NewCompute(env)
	return env, nil
}

func (env *JoyentEnviron) SetName(envName string) {
	env.name = envName
}

func (env *JoyentEnviron) Name() string {
	return env.name
}

func (*JoyentEnviron) Provider() environs.EnvironProvider {
	return providerInstance
}

func (env *JoyentEnviron) SetConfig(cfg *config.Config) error {
	//env.lock.Lock()
	//defer env.lock.Unlock()
	ecfg, err := validateConfig(cfg, env.ecfg)
	if err != nil {
		return err
	}
	//storage, err := newStorage(ecfg)
	//if err != nil {
	//	return err
	//}
	env.ecfg = ecfg
	//env.storage = storage
	return nil
}

func (env *JoyentEnviron) getSnapshot() *JoyentEnviron {
	env.lock.Lock()
	clone := *env
	env.lock.Unlock()
	clone.lock = sync.Mutex{}
	return &clone
}

func (env *JoyentEnviron) Config() *config.Config {
	return env.getSnapshot().ecfg.Config
}

func (env *JoyentEnviron) Storage() storage.Storage {
	return env.getSnapshot().storage
}

func (env *JoyentEnviron) PublicStorage() storage.StorageReader {
	return environs.EmptyStorage
}

func (env *JoyentEnviron) Bootstrap(ctx environs.BootstrapContext, cons constraints.Value) error {
	return common.Bootstrap(ctx, env, cons)
}

func (env *JoyentEnviron) StateInfo() (*state.Info, *api.Info, error) {
	return common.StateInfo(env)
}

func (env *JoyentEnviron) Destroy() error {
	return common.Destroy(env)
}

func (env *JoyentEnviron) Ecfg() *environConfig {
	return env.getSnapshot().ecfg
}

func (env *JoyentEnviron) Credentials() *jpc.Credentials {
	return env.getSnapshot().creds
}

func (env *JoyentEnviron) SetCredentials() {
	env.creds = getCredentials(env)
}

func getCredentials(env *JoyentEnviron) *jpc.Credentials {
	auth := jpc.Auth{User: env.ecfg.mantaUser(), KeyFile: env.ecfg.keyFile(), Algorithm: env.ecfg.algorithm()}

	return &jpc.Credentials{
		UserAuthentication: auth,
		MantaKeyId:         env.ecfg.mantaKeyId(),
		MantaEndpoint:      jpc.Endpoint{URL: env.ecfg.mantaUrl()},
		SdcKeyId:           env.ecfg.sdcKeyId(),
		SdcEndpoint:        jpc.Endpoint{URL: env.ecfg.sdcUrl()},
	}
}

// MetadataLookupParams returns parameters which are used to query simplestreams metadata.
func (env *JoyentEnviron) MetadataLookupParams(region string) (*simplestreams.MetadataLookupParams, error) {
	if region == "" {
		region = env.Ecfg().Region()
	}
	return &simplestreams.MetadataLookupParams{
		Series:        env.Ecfg().DefaultSeries(),
		Region:        region,
		Endpoint:      env.Ecfg().sdcUrl(),
		Architectures: []string{"amd64", "arm"},
	}, nil
}

// Region is specified in the HasRegion interface.
func (env *JoyentEnviron) Region() (simplestreams.CloudSpec, error) {
	return simplestreams.CloudSpec{
		Region:   env.Ecfg().Region(),
		Endpoint: env.Ecfg().sdcUrl(),
	}, nil
}

// GetImageSources returns a list of sources which are used to search for simplestreams image metadata.
func (env *JoyentEnviron) GetImageSources() ([]simplestreams.DataSource, error) {
	// Add the simplestreams source off the control bucket.
	sources := []simplestreams.DataSource{
		storage.NewStorageSimpleStreamsDataSource(env.Storage(), storage.BaseImagesPath)}
	return sources, nil
}

// GetToolsSources returns a list of sources which are used to search for simplestreams tools metadata.
func (env *JoyentEnviron) GetToolsSources() ([]simplestreams.DataSource, error) {
	// Add the simplestreams source off the control bucket.
	sources := []simplestreams.DataSource{
		storage.NewStorageSimpleStreamsDataSource(env.Storage(), storage.BaseToolsPath)}
	return sources, nil
}
