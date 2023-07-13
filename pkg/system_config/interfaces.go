package system_config

type IConfigSyncer interface {

	// StoreImageRegistryConf stores the allowedRegistries and blockedRegistries in the structs representing the
	// registries.conf and policy.json files. It fails if both allowedRegistries and blockedRegistries are set.
	StoreImageRegistryConf(allowedRegistries []string, blockedRegistries []string, insecureRegistries []string) error

	StoreRegistryCerts(registryCertTuples []registryCertTuple) error

	UpdateRegistryMirroringConfig(registry string, mirrors []string) error
	DeleteRegistryMirroringConfig(registry string) error
	CleanupRegistryMirroringConfig() error

	sync() error
}
