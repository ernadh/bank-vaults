// Copyright © 2018 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vault

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/sdk/helper/consts"
	json "github.com/json-iterator/go"
	"github.com/mitchellh/mapstructure"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cast"
	"github.com/spf13/viper"
	corev1 "k8s.io/api/core/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// DefaultConfigFile is the name of the default config file
const DefaultConfigFile = "vault-config.yml"

// Config holds the configuration of the Vault initialization
type Config struct {
	// how many key parts exist
	SecretShares int
	// how many of these parts are needed to unseal Vault (secretThreshold <= secretShares)
	SecretThreshold int

	// if this root token is set, the dynamic generated will be invalidated and this created instead
	InitRootToken string
	// should the root token be stored in the keyStore
	StoreRootToken bool

	// should the KV backend be tested first to validate access rights
	PreFlightChecks bool
}

// vault is an implementation of the Vault interface that will perform actions
// against a Vault server, using a provided KMS to retrieve
type vault struct {
	keyStore    KVService
	cl          *api.Client
	config      *Config
	rotateCache map[string]bool
}

// Interface check
var _ Vault = &vault{}

// Vault is an interface that can be used to attempt to perform actions against
// a Vault server.
type Vault interface {
	Init() error
	RaftInitialized() (bool, error)
	RaftJoin(string) error
	Sealed() (bool, error)
	Active() (bool, error)
	Unseal() error
	Leader() (bool, error)
	LeaderAddress() (string, error)
	Configure(config *viper.Viper) error
}

type purgeUnmanagedConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	Exclude struct {
		Auths    bool `json:"auth,omitempty"`
		Policies bool `json:"policies,omitempty"`
		Secrets  bool `json:"secrets,omitempty"`
	} `json:"exclude,omitempty"`
}

// WIP: This should hold all externalConfig when all sections refactord.
type externalConfig struct {
	PurgeUnmanagedConfig purgeUnmanagedConfig `json:"purgeUnmanagedConfig,omitempty"`
	Auth                 []auth               `json:"auth,omitempty"`
	Policies             []policy             `json:"policies,omitempty"`
	Secrets              []secretEngine       `json:"secrets,omitempty"`
}

var extConfig externalConfig

type KVService interface {
	Set(key string, value []byte) error
	Get(key string) ([]byte, error)
}

type kvTester struct {
	Service KVService
}

func (t kvTester) Test(key string) error {
	_, err := t.Service.Get(key)
	if err != nil {
		if !isNotFoundError(err) {
			return err // nolint:wrapcheck
		}
	}

	return t.Service.Set(key, []byte(key))
}

// New returns a new vault Vault, or an error.
func New(k KVService, cl *api.Client, config Config) (Vault, error) {
	if config.SecretShares < config.SecretThreshold {
		return nil, errors.Errorf("the secret threshold can't be bigger than the shares [%d < %d]", config.SecretShares, config.SecretThreshold)
	}

	return &vault{
		keyStore:    k,
		cl:          cl,
		config:      &config,
		rotateCache: map[string]bool{},
	}, nil
}

func (v *vault) Sealed() (bool, error) {
	resp, err := v.cl.Sys().SealStatus()
	if err != nil {
		return false, errors.Wrap(err, "error checking status")
	}

	return resp.Sealed, nil
}

func (v *vault) Active() (bool, error) {
	req := v.cl.NewRequest("GET", "/v1/sys/health")
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	resp, err := v.cl.RawRequestWithContext(ctx, req)
	if err != nil {
		return false, errors.Wrap(err, "error checking status")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return true, nil
	}

	return false, errors.Errorf("error unexpected status code: %d", resp.StatusCode)
}

func (v *vault) Leader() (bool, error) {
	resp, err := v.cl.Sys().Leader()
	if err != nil {
		return false, errors.Wrap(err, "error checking leader")
	}

	return resp.IsSelf, nil
}

func (v *vault) LeaderAddress() (string, error) {
	resp, err := v.cl.Sys().Leader()
	if err != nil {
		return "", errors.Wrap(err, "error checking leader address")
	}

	return resp.LeaderAddress, nil
}

// Unseal will attempt to unseal vault by retrieving keys from the kms service
// and sending unseal requests to vault. It will return an error if retrieving
// a key fails, or if the unseal progress is reset to 0 (indicating that a key)
// was invalid.
func (v *vault) Unseal() error {
	defer runtime.GC()
	for i := 0; ; i++ {
		keyID := v.unsealKeyForID(i)

		logrus.Debugf("retrieving key from kms service...")
		k, err := v.keyStore.Get(keyID)
		if err != nil {
			return errors.Wrapf(err, "unable to get key '%s'", keyID)
		}

		logrus.Debugf("sending unseal request to vault...")
		resp, err := v.cl.Sys().Unseal(string(k))
		if err != nil {
			return errors.Wrap(err, "fail to send unseal request to vault")
		}

		logrus.Debugf("got unseal response: %+v", *resp)

		if !resp.Sealed {
			return nil
		}

		// if progress is 0, we failed to unseal vault.
		if resp.Progress == 0 {
			return errors.New("failed to unseal vault, are you using the right unseal keys?")
		}
	}
}

type notFoundError interface {
	NotFound() bool
}

func isNotFoundError(err error) bool {
	var notFoundErr notFoundError
	if errors.As(err, &notFoundErr) && notFoundErr.NotFound() {
		return true
	}

	return false
}

func (v *vault) keyStoreNotFound(key string) (bool, error) {
	_, err := v.keyStore.Get(key)
	if isNotFoundError(err) {
		return true, nil
	}

	return false, err // nolint:wrapcheck
}

func (v *vault) keyStoreSet(key string, val []byte) error {
	notFound, err := v.keyStoreNotFound(key)
	if notFound {
		return v.keyStore.Set(key, val)
	} else if err == nil {
		return errors.Errorf("error setting key '%s': it already exists", key)
	} else {
		return errors.Wrapf(err, "error setting key '%s'", key)
	}
}

// Init initializes Vault if is not initialized already
func (v *vault) Init() error {
	initialized, err := v.cl.Sys().InitStatus()
	if err != nil {
		return errors.Wrap(err, "error testing if vault is initialized")
	}
	if initialized {
		logrus.Info("vault is already initialized")

		return nil
	}

	logrus.Info("initializing vault")

	// test backend first
	if v.config.PreFlightChecks {
		tester := kvTester{Service: v.keyStore}
		err = tester.Test(v.testKey())
		if err != nil {
			return errors.Wrap(err, "error testing keystore before init")
		}
	}

	// test for an existing keys
	keys := []string{
		v.rootTokenKey(),
	}

	// add unseal keys
	for i := 0; i <= v.config.SecretShares; i++ {
		keys = append(keys, v.unsealKeyForID(i))
	}

	// test every key
	for _, key := range keys {
		notFound, err := v.keyStoreNotFound(key)
		if notFound && err != nil {
			return errors.Wrapf(err, "error before init: checking key '%s' failed", key)
		} else if !notFound && err == nil {
			return errors.Errorf("error before init: value for key '%s' already exists", key)
		}
	}

	resp, err := v.cl.Sys().Init(&api.InitRequest{
		SecretShares:      v.config.SecretShares,
		SecretThreshold:   v.config.SecretThreshold,
		RecoveryShares:    v.config.SecretShares,
		RecoveryThreshold: v.config.SecretThreshold,
	})
	if err != nil {
		return errors.Wrap(err, "error initializing vault")
	}

	for i, k := range resp.Keys {
		keyID := v.unsealKeyForID(i)
		err := v.keyStoreSet(keyID, []byte(k))
		if err != nil {
			return errors.Wrapf(err, "error storing unseal key '%s'", keyID)
		}

		logrus.WithField("key", keyID).Info("unseal key stored in key store")
	}

	for i, k := range resp.RecoveryKeys {
		keyID := v.recoveryKeyForID(i)
		err := v.keyStoreSet(keyID, []byte(k))
		if err != nil {
			return errors.Wrapf(err, "error storing recovery key '%s'", keyID)
		}

		logrus.WithField("key", keyID).Info("recovery key stored in key store")
	}

	rootToken := resp.RootToken

	// this sets up a predefined root token
	if v.config.InitRootToken != "" {
		logrus.Info("setting up init root token, waiting for vault to be unsealed")

		wait := time.Second * 2
		for {
			sealed, err := v.Sealed()
			if !sealed {
				break
			}
			if err == nil {
				logrus.Info("vault still sealed, wait for unsealing")
			} else {
				logrus.Infof("vault not reachable: %s", err.Error())
			}

			time.Sleep(wait)
		}

		// use temporary token
		v.cl.SetToken(resp.RootToken)

		// setup root token with provided key
		_, err := v.cl.Auth().Token().CreateOrphan(&api.TokenCreateRequest{
			ID:          v.config.InitRootToken,
			Policies:    []string{"root"},
			DisplayName: "root-token",
			NoParent:    true,
		})
		if err != nil {
			return errors.Wrapf(err, "unable to setup requested root token, (temporary root token: '%s')", resp.RootToken)
		}

		// revoke the temporary token
		err = v.cl.Auth().Token().RevokeSelf(resp.RootToken)
		if err != nil {
			return errors.Wrap(err, "unable to revoke temporary root token")
		}

		rootToken = v.config.InitRootToken
	}

	if v.config.StoreRootToken {
		rootTokenKey := v.rootTokenKey()
		if err = v.keyStoreSet(rootTokenKey, []byte(resp.RootToken)); err != nil {
			return errors.Wrapf(err, "error storing root token '%s' in key'%s'", rootToken, rootTokenKey)
		}
		logrus.WithField("key", rootTokenKey).Info("root token stored in key store")
	} else if v.config.InitRootToken == "" {
		logrus.WithField("root-token", resp.RootToken).Warnf("won't store root token in key store, this token grants full privileges to vault, so keep this secret")
	}

	return nil
}

// in our case Vault is initialized when root key is stored in the Cloud KMS
func (v *vault) RaftInitialized() (bool, error) {
	rootToken, err := v.keyStore.Get(v.rootTokenKey())
	if err != nil {
		if isNotFoundError(err) {
			return false, nil
		}

		return false, errors.Wrapf(err, "unable to get key '%s'", v.rootTokenKey())
	}

	if len(rootToken) > 0 {
		return true, nil
	}

	return false, nil
}

// RaftJoin joins Vault raft cluster if is not initialized already
func (v *vault) RaftJoin(leaderAPIAddr string) error {
	// raft storage mode
	if leaderAPIAddr != "" {
		initialized, err := v.cl.Sys().InitStatus()
		if err != nil {
			return errors.Wrap(err, "error testing if vault is initialized")
		}

		if initialized {
			logrus.Info("vault is already initialized, skipping raft join")

			return nil
		}
	} else if strings.HasSuffix(os.Getenv("POD_NAME"), "-0") {
		// raft ha_storage mode
		// TODO this currently doesn't allow multi-DC setups with Raft HA storage only mode
		return nil
	}

	request := api.RaftJoinRequest{
		LeaderAPIAddr: leaderAPIAddr,
	}

	raftCacertFile := os.Getenv("VAULT_RAFT_CACERT")
	if raftCacertFile == "" {
		raftCacertFile = os.Getenv(api.EnvVaultCACert)
	}

	if raftCacertFile != "" {
		leaderCACert, err := ioutil.ReadFile(raftCacertFile)
		if err != nil {
			return errors.Wrap(err, "error reading vault raft CA certificate")
		}

		request.LeaderCACert = string(leaderCACert)
	}

	response, err := v.cl.Sys().RaftJoin(&request)
	if err != nil {
		return errors.Wrap(err, "error joining raft cluster")
	}

	if response.Joined {
		logrus.Info("vault joined raft cluster")

		return nil
	}

	return errors.New("vault hasn't joined raft cluster")
}

func (v *vault) Configure(config *viper.Viper) error {
	logrus.Debugf("retrieving key from kms service...")

	rootToken, err := v.keyStore.Get(v.rootTokenKey())
	if err != nil {
		return errors.Wrapf(err, "unable to get key '%s'", v.rootTokenKey())
	}

	v.cl.SetToken(string(rootToken))

	// Clear the token and GC it
	defer runtime.GC()
	defer v.cl.SetToken("")
	defer func() { rootToken = nil }()

	err = config.Unmarshal(&extConfig)
	if err != nil {
		return errors.Wrap(err, "error loading externalConfig")
	}

	if err = v.configureAuthMethods(); err != nil {
		return errors.Wrap(err, "error configuring auth methods for vault")
	}

	if err = v.configurePolicies(); err != nil {
		return errors.Wrap(err, "error configuring policies for vault")
	}

	if err = v.configureSecretsEngines(); err != nil {
		return errors.Wrap(err, "error configuring secret engines for vault")
	}

	err = v.configurePlugins(config)
	if err != nil {
		return errors.Wrap(err, "error configuring plugins for vault")
	}

	err = v.configureAuditDevices(config)
	if err != nil {
		return errors.Wrap(err, "error configuring audit devices for vault")
	}

	err = v.configureStartupSecrets(config)
	if err != nil {
		return errors.Wrap(err, "error writing startup secrets to vault")
	}

	err = v.configureIdentityGroups(config)
	if err != nil {
		return errors.Wrap(err, "error writing groups configurations for vault")
	}

	return err
}

func (*vault) unsealKeyForID(i int) string {
	return fmt.Sprint("vault-unseal-", i)
}

func (*vault) recoveryKeyForID(i int) string {
	return fmt.Sprint("vault-recovery-", i)
}

func (*vault) rootTokenKey() string {
	return "vault-root"
}

func (*vault) testKey() string {
	return "vault-test"
}

func (v *vault) configurePlugins(config *viper.Viper) error {
	plugins := []map[string]interface{}{}
	err := config.UnmarshalKey("plugins", &plugins)
	if err != nil {
		return errors.Wrap(err, "error unmarshalling vault plugins config")
	}

	if len(plugins) == 0 {
		return nil
	}

	listPlugins, err := v.cl.Sys().ListPlugins(&api.ListPluginsInput{})
	if err != nil {
		return errors.Wrap(err, "failed to retrieve list of plugins")
	}

	logrus.Debugf("already registered plugins: %#v", listPlugins.PluginsByType)

	for _, plugin := range plugins {
		command, err := getOrError(plugin, "command")
		if err != nil {
			return errors.Wrap(err, "error getting command for plugin")
		}
		pluginName, err := getOrError(plugin, "plugin_name")
		if err != nil {
			return errors.Wrap(err, "error getting plugin_name for plugin")
		}
		sha256, err := getOrError(plugin, "sha256")
		if err != nil {
			return errors.Wrap(err, "error getting sha256 for plugin")
		}
		typeRaw, err := getOrError(plugin, "type")
		if err != nil {
			return errors.Wrap(err, "error getting type for plugin")
		}
		pluginType, err := consts.ParsePluginType(typeRaw)
		if err != nil {
			return errors.Wrap(err, "error parsing type for plugin")
		}

		input := api.RegisterPluginInput{
			Name:    pluginName,
			Command: command,
			SHA256:  sha256,
			Type:    pluginType,
		}
		logrus.Infof("registering plugin with input: %#v", input)

		err = v.cl.Sys().RegisterPlugin(&input)
		if err != nil {
			return errors.Wrapf(err, "error registering plugin %s in vault", pluginName)
		}

		logrus.Infoln("registered plugin", pluginName)
	}

	return nil
}

func (v *vault) configureAuditDevices(config *viper.Viper) error {
	auditDevices := []map[string]interface{}{}
	err := config.UnmarshalKey("audit", &auditDevices)
	if err != nil {
		return errors.Wrap(err, "error unmarshalling audit devices config")
	}

	for _, auditDevice := range auditDevices {
		auditDeviceType, err := cast.ToStringE(auditDevice["type"])
		if err != nil {
			return errors.Wrap(err, "error finding type for audit device")
		}

		path := auditDeviceType
		if pathOverwrite, ok := auditDevice["path"]; ok {
			path, err = cast.ToStringE(pathOverwrite)
			if err != nil {
				return errors.Wrap(err, "error converting path for audit device")
			}
			path = strings.Trim(path, "/")
		}

		mounts, err := v.cl.Sys().ListAudit()
		if err != nil {
			return errors.Wrap(err, "error reading audit mounts from vault")
		}

		logrus.Infof("already existing audit devices: %#v", mounts)

		if mounts[path+"/"] == nil {
			var options api.EnableAuditOptions
			err = mapstructure.Decode(auditDevice, &options)
			if err != nil {
				return errors.Wrap(err, "error parsing audit options")
			}
			logrus.Infof("enabling audit device with options: %#v", options)
			err = v.cl.Sys().EnableAuditWithOptions(path, &options)
			if err != nil {
				return errors.Wrapf(err, "error enabling audit device %s in vault", path)
			}

			logrus.Infoln("mounted audit device", auditDeviceType, "to", path)
		} else {
			logrus.Infof("audit device is already mounted: %s/", path)
		}
	}

	return nil
}

func (v *vault) configureStartupSecrets(config *viper.Viper) error {
	raw := config.Get("startupSecrets")
	startupSecrets, err := toSliceStringMapE(raw)
	if err != nil {
		return errors.Wrapf(err, "error decoding data for startup secrets")
	}
	for _, startupSecret := range startupSecrets {
		startupSecretType, err := cast.ToStringE(startupSecret["type"])
		if err != nil {
			return errors.Wrap(err, "error finding type for startup secret")
		}

		switch startupSecretType {
		case "kv":
			path, data, err := readStartupSecret(startupSecret)
			if err != nil {
				return errors.Wrap(err, "unable to read 'kv' startup secret")
			}

			_, err = v.writeWithWarningCheck(path, data)
			if err != nil {
				return errors.Wrapf(err, "error writing data for startup 'kv' secret '%s'", path)
			}

		case "pki":
			path, data, err := readStartupSecret(startupSecret)
			if err != nil {
				return errors.Wrap(err, "unable to read 'pki' startup secret")
			}

			certData, err := generateCertPayload(data["data"])
			if err != nil {
				return errors.Wrap(err, "error generating 'pki' startup secret")
			}

			_, err = v.writeWithWarningCheck(path, certData)
			if err != nil {
				return errors.Wrapf(err, "error writing data for startup 'pki' secret '%s'", path)
			}

		default:
			return errors.Errorf("'%s' startup secret type is not supported, only 'kv' or 'pki'", startupSecretType)
		}
	}

	return nil
}

func (v *vault) writeWithWarningCheck(path string, data map[string]interface{}) (*api.Secret, error) {
	sec, err := v.cl.Logical().Write(path, data)
	if err != nil {
		return nil, err
	}
	if sec != nil {
		for _, warning := range sec.Warnings {
			logrus.Warn(warning)
		}
	}
	return sec, nil
}

func readStartupSecret(startupSecret map[string]interface{}) (string, map[string]interface{}, error) {
	path, err := cast.ToStringE(startupSecret["path"])
	if err != nil {
		return "", nil, errors.Wrap(err, "error findind path for startup secret")
	}

	data, err := getOrDefaultStringMap(startupSecret, "data")
	if err != nil {
		return "", nil, errors.Wrapf(err, "error getting data for startup secret '%s'", path)
	}

	if _, ok := data["secretKeyRef"]; ok {
		secData, err := getOrDefaultSecretData(data["secretKeyRef"])
		if err != nil {
			return "", nil, errors.Wrap(err, "error getting data from k8s secret")
		}
		data = secData
	}

	return path, data, nil
}

func generateCertPayload(data interface{}) (map[string]interface{}, error) {
	pkiData, err := cast.ToStringMapStringE(data)
	if err != nil {
		return map[string]interface{}{}, errors.Wrapf(err, "cast to map[string]... failed: %v", data)
	}

	pkiSlice := []string{}
	for _, v := range pkiData {
		pkiSlice = append(pkiSlice, v)
	}

	if len(pkiSlice) < 2 {
		return map[string]interface{}{}, errors.Errorf("missing key or certificate in pki data: %v", pkiData)
	}

	return map[string]interface{}{"pem_bundle": strings.Join(pkiSlice, "\n")}, nil
}

func readVaultGroup(group string, client *api.Client) (secret *api.Secret, err error) {
	secret, err = client.Logical().Read(fmt.Sprintf("identity/group/name/%s", group))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read group %s by name", group)
	}
	if secret == nil {
		// No Data returned, Group does not exist
		return nil, nil
	}
	return secret, nil
}

func readVaultGroupAlias(id string, client *api.Client) (secret *api.Secret, err error) {
	secret, err = client.Logical().Read(fmt.Sprintf("identity/group-alias/id/%s", id))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read group alias by id %s", id)
	}
	if secret == nil {
		// No Data returned, Group does not exist
		return nil, nil
	}
	return secret, nil
}

func getVaultAuthMountAccessor(path string, client *api.Client) (accessor string, err error) {
	path = strings.TrimRight(path, "/") + "/"
	mounts, err := client.Sys().ListAuth()
	if err != nil {
		return "", errors.Wrapf(err, "failed to read auth mounts from vault")
	}
	if mounts[path] == nil {
		return "", errors.Errorf("auth mount path %s does not exist in vault", path)
	}
	return mounts[path].Accessor, nil
}

func getVaultGroupID(group string, client *api.Client) (id string, err error) {
	g, err := readVaultGroup(group, client)
	if err != nil {
		return "", errors.Wrapf(err, "error reading group %s", group)
	}
	if g == nil {
		return "", errors.Errorf("group %s does not exist", group)
	}
	return g.Data["id"].(string), nil
}

func getVaultGroupAliasName(aliasID string, client *api.Client) (id string, err error) {
	alias, err := readVaultGroupAlias(aliasID, client)
	if err != nil {
		return "", errors.Wrapf(err, "error reading group alias %s", aliasID)
	}
	if alias == nil {
		return "", errors.Errorf("group alias %s does not exist", aliasID)
	}
	return alias.Data["name"].(string), nil
}

func getVaultGroupAliasMount(aliasID string, client *api.Client) (id string, err error) {
	alias, err := readVaultGroupAlias(aliasID, client)
	if err != nil {
		return "", errors.Wrapf(err, "error reading group alias %s", aliasID)
	}
	if alias == nil {
		return "", errors.Errorf("group alias %s does not exist", aliasID)
	}
	return alias.Data["mount_accessor"].(string), nil
}

func findVaultGroupAliasIDFromNameAndMount(name string, accessor string, client *api.Client) (id string, err error) {
	aliases, err := client.Logical().List("identity/group-alias/id")
	if err != nil {
		return "", errors.Wrap(err, "error listing group aliases")
	}
	if aliases == nil {
		return "", nil
	}

	for _, alias := range aliases.Data["keys"].([]interface{}) {
		aliasName, err := getVaultGroupAliasName(cast.ToString(alias), client)
		if err != nil {
			return "", errors.Wrapf(err, "error fetching name for alias id: %s err", alias)
		}

		aliasMount, err := getVaultGroupAliasMount(cast.ToString(alias), client)
		if err != nil {
			return "", errors.Wrapf(err, "error fetching mount for alias id: %s err", alias)
		}

		if aliasName == name && aliasMount == accessor {
			return cast.ToString(alias), nil
		}
	}

	// Did not find any alias matching Name and MountPath
	return "", nil
}

func (v *vault) configureIdentityGroups(config *viper.Viper) error {
	groups := []map[string]interface{}{}
	groupAliases := []map[string]interface{}{}

	err := config.UnmarshalKey("groups", &groups)
	if err != nil {
		return errors.Wrap(err, "error unmarshalling vault groups config")
	}

	err = config.UnmarshalKey("group-aliases", &groupAliases)
	if err != nil {
		return errors.Wrap(err, "error unmarshalling vault group aliases config")
	}

	for _, group := range groups {
		g, err := readVaultGroup(cast.ToString(group["name"]), v.cl)
		if err != nil {
			return errors.Wrap(err, "error reading group")
		}

		// Currently does not support specifying members directly in the group config
		// Use group aliases for that
		if cast.ToString(group["type"]) != "external" {
			return errors.Errorf("only external groups are supported for now")
		}

		config := map[string]interface{}{
			"name":     cast.ToString(group["name"]),
			"type":     cast.ToString(group["type"]),
			"policies": cast.ToStringSlice(group["policies"]),
			"metadata": cast.ToStringMap(group["metadata"]),
		}

		if g == nil {
			logrus.Infof("creating group: %s", group["name"])
			_, err = v.writeWithWarningCheck("identity/group", config)
			if err != nil {
				return errors.Wrapf(err, "failed to create group %s", group["name"])
			}
		} else {
			logrus.Infof("tuning already existing group: %s", group["name"])
			_, err = v.writeWithWarningCheck(fmt.Sprintf("identity/group/name/%s", group["name"]), config)
			if err != nil {
				return errors.Wrapf(err, "failed to tune group %s", group["name"])
			}
		}
	}

	// Group Aliases for External Groups might require to have the same Name when on different Mount/Path combinations
	// external groups can only have ONE alias so we need to make sure not to overwrite any
	for _, groupAlias := range groupAliases {
		accessor, err := getVaultAuthMountAccessor(cast.ToString(groupAlias["mountpath"]), v.cl)
		if err != nil {
			return errors.Wrapf(err, "error getting mount accessor for %s", groupAlias["mountpath"])
		}

		id, err := getVaultGroupID(cast.ToString(groupAlias["group"]), v.cl)
		if err != nil {
			return errors.Wrapf(err, "error getting canonical_id for group %s", groupAlias["group"])
		}

		config := map[string]interface{}{
			"name":           cast.ToString(groupAlias["name"]),
			"mount_accessor": accessor,
			"canonical_id":   id,
		}

		// Find a matching alias for NAME and MOUNT
		ga, err := findVaultGroupAliasIDFromNameAndMount(cast.ToString(groupAlias["name"]), accessor, v.cl)
		if err != nil {
			return errors.Wrapf(err, "error finding group-alias %s", groupAlias["name"])
		}

		if ga == "" {
			logrus.Infof("creating group-alias: %s@%s", groupAlias["name"], accessor)
			_, err = v.writeWithWarningCheck("identity/group-alias", config)
			if err != nil {
				return errors.Wrapf(err, "failed to create group-alias %s", groupAlias["name"])
			}
		} else {
			logrus.Infof("tuning already existing group-alias: %s@%s - ID: %s", groupAlias["name"], accessor, ga)
			_, err = v.writeWithWarningCheck(fmt.Sprintf("identity/group-alias/id/%s", ga), config)
			if err != nil {
				return errors.Wrapf(err, "failed to tune group-alias %s", ga)
			}
		}
	}

	return nil
}

// toSliceStringMapE casts []map[string]interface{} preserving nested types
func toSliceStringMapE(o interface{}) ([]map[string]interface{}, error) {
	data, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	var sm []map[string]interface{}
	return sm, json.Unmarshal(data, &sm)
}

func getOrDefaultStringMap(m map[string]interface{}, key string) (map[string]interface{}, error) {
	value := m[key]
	if value != nil {
		return cast.ToStringMapE(value)
	}
	return map[string]interface{}{}, nil
}

func getOrError(m map[string]interface{}, key string) (string, error) {
	value := m[key]
	if value != nil {
		return cast.ToStringE(value)
	}
	return "", errors.Errorf("value for %s is not set", key)
}

func isOverwriteProhibitedError(err error) bool {
	return strings.Contains(err.Error(), "delete them before reconfiguring")
}

func getOrDefaultSecretData(m interface{}) (map[string]interface{}, error) {
	values, err := cast.ToSliceE(m)
	if err != nil {
		return map[string]interface{}{}, err
	}

	k8sCfg := crconfig.GetConfigOrDie()
	c, err := crclient.New(k8sCfg, crclient.Options{})
	if err != nil {
		return map[string]interface{}{}, err
	}

	vaultNamespace := os.Getenv("NAMESPACE")

	secData := map[string]string{}
	for _, value := range values {
		keyRef, err := cast.ToStringMapStringE(value)
		if err != nil {
			return map[string]interface{}{}, err
		}

		secret := &corev1.Secret{}
		err = c.Get(context.Background(), crclient.ObjectKey{
			Namespace: vaultNamespace,
			Name:      keyRef["name"],
		}, secret)
		if err != nil {
			return map[string]interface{}{}, err
		}
		secData[keyRef["key"]] = cast.ToString(secret.Data[keyRef["key"]])
	}
	data := map[string]interface{}{}
	data["data"] = secData

	return data, nil
}
