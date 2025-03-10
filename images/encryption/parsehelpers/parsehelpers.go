/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

// Package parsehelpers provides parse helpers for CLI applications.
// This package does not depend on any specific CLI library such as github.com/urfave/cli .
package parsehelpers

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gobars/ocicrypt"
	encconfig "github.com/gobars/ocicrypt/config"
	"github.com/gobars/ocicrypt/config/pkcs11config"
	"github.com/gobars/ocicrypt/crypto/pkcs11"
	encutils "github.com/gobars/ocicrypt/utils"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type EncArgs struct {
	GPGHomedir   string   // --gpg-homedir
	GPGVersion   string   // --gpg-version
	Key          []string // --key
	Recipient    []string // --recipient
	DecRecipient []string // --dec-recipient
}

// processRecipientKeys sorts the array of recipients by type. Recipients may be either
// x509 certificates, public keys, or PGP public keys identified by email address or name
func processRecipientKeys(recipients []string) ([][]byte, [][]byte, [][]byte, [][]byte, [][]byte, [][]byte, error) {
	var (
		gpgRecipients [][]byte
		pubkeys       [][]byte
		x509s         [][]byte
		pkcs11Pubkeys [][]byte
		pkcs11Yamls   [][]byte
		keyProvider   [][]byte
	)

	for _, recipient := range recipients {

		idx := strings.Index(recipient, ":")
		if idx < 0 {
			return nil, nil, nil, nil, nil, nil, errors.New("invalid recipient format")
		}

		protocol := recipient[:idx]
		value := recipient[idx+1:]

		switch protocol {
		case "pgp":
			gpgRecipients = append(gpgRecipients, []byte(value))

		case "jwe":
			tmp, err := os.ReadFile(value)
			if err != nil {
				return nil, nil, nil, nil, nil, nil, fmt.Errorf("unable to read file: %w", err)
			}
			if !encutils.IsPublicKey(tmp) {
				return nil, nil, nil, nil, nil, nil, errors.New("file provided is not a public key")
			}
			pubkeys = append(pubkeys, tmp)

		case "pkcs7":
			tmp, err := os.ReadFile(value)
			if err != nil {
				return nil, nil, nil, nil, nil, nil, fmt.Errorf("unable to read file %s: %w", value, err)
			}
			if !encutils.IsCertificate(tmp) {
				return nil, nil, nil, nil, nil, nil, errors.New("file provided is not an x509 cert")
			}
			x509s = append(x509s, tmp)

		case "pkcs11":
			tmp, err := os.ReadFile(value)
			if err != nil {
				return nil, nil, nil, nil, nil, nil, fmt.Errorf("unable to read file %s: %w", value, err)
			}
			if encutils.IsPkcs11PublicKey(tmp) {
				pkcs11Yamls = append(pkcs11Yamls, tmp)
			} else if encutils.IsPublicKey(tmp) {
				pkcs11Pubkeys = append(pkcs11Pubkeys, tmp)
			} else {
				return nil, nil, nil, nil, nil, nil, errors.New("provided file is not a public key")
			}

		case "provider":
			keyProvider = append(keyProvider, []byte(value))

		default:
			return nil, nil, nil, nil, nil, nil, errors.New("provided protocol not recognized")
		}
	}
	return gpgRecipients, pubkeys, x509s, pkcs11Pubkeys, pkcs11Yamls, keyProvider, nil
}

// processPwdString process a password that may be in any of the following formats:
// - file=<passwordfile>
// - pass=<password>
// - fd=<filedescriptor>
// - <password>
func processPwdString(pwdString string) ([]byte, error) {
	if strings.HasPrefix(pwdString, "file=") {
		return os.ReadFile(pwdString[5:])
	} else if strings.HasPrefix(pwdString, "pass=") {
		return []byte(pwdString[5:]), nil
	} else if strings.HasPrefix(pwdString, "fd=") {
		fdStr := pwdString[3:]
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			return nil, fmt.Errorf("could not parse file descriptor %s: %w", fdStr, err)
		}
		f := os.NewFile(uintptr(fd), "pwdfile")
		if f == nil {
			return nil, fmt.Errorf("%s is not a valid file descriptor", fdStr)
		}
		defer f.Close()
		pwd := make([]byte, 64)
		n, err := f.Read(pwd)
		if err != nil {
			return nil, fmt.Errorf("could not read from file descriptor: %w", err)
		}
		return pwd[:n], nil
	}
	return []byte(pwdString), nil
}

// processPrivateKeyFiles sorts the different types of private key files; private key files may either be
// private keys or GPG private key ring files. The private key files may include the password for the
// private key and take any of the following forms:
// - <filename>
// - <filename>:file=<passwordfile>
// - <filename>:pass=<password>
// - <filename>:fd=<filedescriptor>
// - <filename>:<password>
// - keyprovider:<...>
func processPrivateKeyFiles(keyFilesAndPwds []string) ([][]byte, [][]byte, [][]byte, [][]byte, [][]byte, [][]byte, error) {
	var (
		gpgSecretKeyRingFiles [][]byte
		gpgSecretKeyPasswords [][]byte
		privkeys              [][]byte
		privkeysPasswords     [][]byte
		pkcs11Yamls           [][]byte
		keyProviders          [][]byte
		err                   error
	)
	// keys needed for decryption in case of adding a recipient
	for _, keyfileAndPwd := range keyFilesAndPwds {
		var password []byte

		// treat "provider" protocol separately
		if strings.HasPrefix(keyfileAndPwd, "provider:") {
			keyProviders = append(keyProviders, []byte(keyfileAndPwd[9:]))
			continue
		}
		parts := strings.Split(keyfileAndPwd, ":")
		if len(parts) == 2 {
			password, err = processPwdString(parts[1])
			if err != nil {
				return nil, nil, nil, nil, nil, nil, err
			}
		}

		keyfile := parts[0]
		tmp, err := os.ReadFile(keyfile)
		if err != nil {
			return nil, nil, nil, nil, nil, nil, err
		}
		isPrivKey, err := encutils.IsPrivateKey(tmp, password)
		if encutils.IsPasswordError(err) {
			return nil, nil, nil, nil, nil, nil, err
		}

		if encutils.IsPkcs11PrivateKey(tmp) {
			pkcs11Yamls = append(pkcs11Yamls, tmp)
		} else if isPrivKey {
			privkeys = append(privkeys, tmp)
			privkeysPasswords = append(privkeysPasswords, password)
		} else if encutils.IsGPGPrivateKeyRing(tmp) {
			gpgSecretKeyRingFiles = append(gpgSecretKeyRingFiles, tmp)
			gpgSecretKeyPasswords = append(gpgSecretKeyPasswords, password)
		} else {
			return nil, nil, nil, nil, nil, nil, fmt.Errorf("unidentified private key in file %s", keyfile)
		}
	}
	return gpgSecretKeyRingFiles, gpgSecretKeyPasswords, privkeys, privkeysPasswords, pkcs11Yamls, keyProviders, nil
}

func CreateGPGClient(args EncArgs) (ocicrypt.GPGClient, error) {
	return ocicrypt.NewGPGClient(args.GPGVersion, args.GPGHomedir)
}

func getGPGPrivateKeys(args EncArgs, gpgSecretKeyRingFiles [][]byte, descs []ocispec.Descriptor, mustFindKey bool) (gpgPrivKeys [][]byte, gpgPrivKeysPwds [][]byte, err error) {
	gpgClient, err := CreateGPGClient(args)
	if err != nil {
		return nil, nil, err
	}

	var gpgVault ocicrypt.GPGVault
	if len(gpgSecretKeyRingFiles) > 0 {
		gpgVault = ocicrypt.NewGPGVault()
		err = gpgVault.AddSecretKeyRingDataArray(gpgSecretKeyRingFiles)
		if err != nil {
			return nil, nil, err
		}
	}
	return ocicrypt.GPGGetPrivateKey(descs, gpgClient, gpgVault, mustFindKey)
}

// CreateDecryptCryptoConfig creates the CryptoConfig object that contains the necessary
// information to perform decryption from command line options and possibly
// LayerInfos describing the image and helping us to query for the PGP decryption keys
func CreateDecryptCryptoConfig(args EncArgs, descs []ocispec.Descriptor) (encconfig.CryptoConfig, error) {
	ccs := []encconfig.CryptoConfig{}

	// x509 cert is needed for PKCS7 decryption
	_, _, x509s, _, _, _, err := processRecipientKeys(args.DecRecipient)
	if err != nil {
		return encconfig.CryptoConfig{}, err
	}

	gpgSecretKeyRingFiles, gpgSecretKeyPasswords, privKeys, privKeysPasswords, pkcs11Yamls, keyProviders, err := processPrivateKeyFiles(args.Key)
	if err != nil {
		return encconfig.CryptoConfig{}, err
	}

	_, err = CreateGPGClient(args)
	gpgInstalled := err == nil
	if gpgInstalled {
		if len(gpgSecretKeyRingFiles) == 0 && len(privKeys) == 0 && len(pkcs11Yamls) == 0 && len(keyProviders) == 0 && descs != nil {
			// Get pgp private keys from keyring only if no private key was passed
			gpgPrivKeys, gpgPrivKeyPasswords, err := getGPGPrivateKeys(args, gpgSecretKeyRingFiles, descs, true)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}

			gpgCc, err := encconfig.DecryptWithGpgPrivKeys(gpgPrivKeys, gpgPrivKeyPasswords)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			ccs = append(ccs, gpgCc)

		} else if len(gpgSecretKeyRingFiles) > 0 {
			gpgCc, err := encconfig.DecryptWithGpgPrivKeys(gpgSecretKeyRingFiles, gpgSecretKeyPasswords)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			ccs = append(ccs, gpgCc)

		}
	}

	if len(x509s) > 0 {
		x509sCc, err := encconfig.DecryptWithX509s(x509s)
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		ccs = append(ccs, x509sCc)
	}
	if len(privKeys) > 0 {
		privKeysCc, err := encconfig.DecryptWithPrivKeys(privKeys, privKeysPasswords)
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		ccs = append(ccs, privKeysCc)
	}
	if len(pkcs11Yamls) > 0 {
		p11conf, err := pkcs11config.GetUserPkcs11Config()
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		pkcs11PrivKeysCc, err := encconfig.DecryptWithPkcs11Yaml(p11conf, pkcs11Yamls)
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		ccs = append(ccs, pkcs11PrivKeysCc)
	}
	if len(keyProviders) > 0 {
		keyProviderCc, err := encconfig.DecryptWithKeyProvider(keyProviders)
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		ccs = append(ccs, keyProviderCc)
	}
	return encconfig.CombineCryptoConfigs(ccs), nil
}

// CreateCryptoConfig from the list of recipient strings and list of key paths of private keys
func CreateCryptoConfig(args EncArgs, descs []ocispec.Descriptor) (encconfig.CryptoConfig, error) {
	recipients := args.Recipient
	keys := args.Key

	var decryptCc *encconfig.CryptoConfig
	ccs := []encconfig.CryptoConfig{}
	if len(keys) > 0 {
		dcc, err := CreateDecryptCryptoConfig(args, descs)
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		decryptCc = &dcc
		ccs = append(ccs, dcc)
	}

	if len(recipients) > 0 {
		gpgRecipients, pubKeys, x509s, pkcs11Pubkeys, pkcs11Yamls, keyProvider, err := processRecipientKeys(recipients)
		if err != nil {
			return encconfig.CryptoConfig{}, err
		}
		encryptCcs := []encconfig.CryptoConfig{}

		gpgClient, err := CreateGPGClient(args)
		gpgInstalled := err == nil
		if len(gpgRecipients) > 0 && gpgInstalled {
			gpgPubRingFile, err := gpgClient.ReadGPGPubRingFile()
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}

			gpgCc, err := encconfig.EncryptWithGpg(gpgRecipients, gpgPubRingFile)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			encryptCcs = append(encryptCcs, gpgCc)
		}

		// Create Encryption Crypto Config
		if len(x509s) > 0 {
			pkcs7Cc, err := encconfig.EncryptWithPkcs7(x509s)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			encryptCcs = append(encryptCcs, pkcs7Cc)
		}
		if len(pubKeys) > 0 {
			jweCc, err := encconfig.EncryptWithJwe(pubKeys)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			encryptCcs = append(encryptCcs, jweCc)
		}
		var p11conf *pkcs11.Pkcs11Config
		if len(pkcs11Yamls) > 0 || len(pkcs11Pubkeys) > 0 {
			p11conf, err = pkcs11config.GetUserPkcs11Config()
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			pkcs11Cc, err := encconfig.EncryptWithPkcs11(p11conf, pkcs11Pubkeys, pkcs11Yamls)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			encryptCcs = append(encryptCcs, pkcs11Cc)
		}

		if len(keyProvider) > 0 {
			keyProviderCc, err := encconfig.EncryptWithKeyProvider(keyProvider)
			if err != nil {
				return encconfig.CryptoConfig{}, err
			}
			encryptCcs = append(encryptCcs, keyProviderCc)
		}
		ecc := encconfig.CombineCryptoConfigs(encryptCcs)
		if decryptCc != nil {
			ecc.EncryptConfig.AttachDecryptConfig(decryptCc.DecryptConfig)
		}
		ccs = append(ccs, ecc)
	}

	if len(ccs) > 0 {
		return encconfig.CombineCryptoConfigs(ccs), nil
	}
	return encconfig.CryptoConfig{}, nil
}
