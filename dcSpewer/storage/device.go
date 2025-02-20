package storage

import (
	"crypto/rsa"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
)

type EmulatorDevice struct {
	// The asset.DeviceRegistration moid of this emulator.

	Moid string
	// Private RSA Key
	PrivateKeyPem string
	PrivateKey    *rsa.PrivateKey `json:"-"`

	// Key ID
	AccessKeyId string
	// Device identifier, e.g. serial
	Identifier []string
	// Parent device id
	ParentId string
	// Device configuration
	Config config.DeviceConfiguration
}
