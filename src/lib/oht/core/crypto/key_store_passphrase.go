package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"lib/oht/core/common"
	"lib/oht/core/crypto/randentropy"

	"github.com/pborman/uuid"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/crypto/scrypt"
)

const (
	keyHeaderKDF = "scrypt"
	KDFStandard  = iota
	KDFLight     = iota

	// n,r,p = 2^18, 8, 1 uses 256MB memory and approx 1s CPU time on a modern CPU.
	stdScryptN = 1 << 18
	stdScryptP = 1

	// n,r,p = 2^12, 8, 6 uses 4MB memory and approx 100ms CPU time on a modern CPU.
	lightScryptN = 1 << 12
	lightScryptP = 6

	scryptR     = 8
	scryptDKLen = 32
)

type keyStorePassphrase struct {
	keysDirPath string
	scryptN     int
	scryptP     int
	scryptR     int
	scryptDKLen int
}

func NewKeyStorePassphrase(path string, safety int) KeyStore {
	if safety == KDFStandard {
		return &keyStorePassphrase{path, stdScryptN, stdScryptP, scryptR, scryptDKLen}
	} else {
		return &keyStorePassphrase{path, lightScryptN, lightScryptP, scryptR, scryptDKLen}
	}
}

func (ks keyStorePassphrase) GenerateNewKey(rand io.Reader, auth string) (key *Key, err error) {
	return GenerateNewKeyDefault(ks, rand, auth)
}

func (ks keyStorePassphrase) GetKey(keyAddr common.Address, auth string) (key *Key, err error) {
	keyBytes, keyId, err := decryptKeyFromFile(ks.keysDirPath, keyAddr, auth)
	if err == nil {
		key = &Key{
			Id:         uuid.UUID(keyId),
			Address:    keyAddr,
			PrivateKey: ToECDSA(keyBytes),
		}
	}
	return
}

func (ks keyStorePassphrase) Cleanup(keyAddr common.Address) (err error) {
	return cleanup(ks.keysDirPath, keyAddr)
}

func (ks keyStorePassphrase) GetKeyAddresses() (addresses []common.Address, err error) {
	return getKeyAddresses(ks.keysDirPath)
}

func (ks keyStorePassphrase) StoreKey(key *Key, auth string) (err error) {
	authArray := []byte(auth)
	salt := randentropy.GetEntropyCSPRNG(32)
	//now := time.Now()
	derivedKey, err := scrypt.Key(authArray, salt, ks.scryptN, ks.scryptR, ks.scryptP, ks.scryptDKLen)
	if err != nil {
		return err
	}
	//fmt.Println("took: ", time.Since(now))
	encryptKey := derivedKey[:16]
	keyBytes := FromECDSA(key.PrivateKey)

	iv := randentropy.GetEntropyCSPRNG(aes.BlockSize) // 16
	cipherText, err := aesCTRXOR(encryptKey, keyBytes, iv)
	if err != nil {
		return err
	}

	mac := Sha3(derivedKey[16:32], cipherText)

	scryptParamsJSON := make(map[string]interface{}, 5)
	scryptParamsJSON["n"] = ks.scryptN
	scryptParamsJSON["r"] = ks.scryptR
	scryptParamsJSON["p"] = ks.scryptP
	scryptParamsJSON["dklen"] = ks.scryptDKLen
	scryptParamsJSON["salt"] = hex.EncodeToString(salt)

	cipherParamsJSON := cipherparamsJSON{
		IV: hex.EncodeToString(iv),
	}

	cryptoStruct := cryptoJSON{
		Cipher:       "aes-128-ctr",
		CipherText:   hex.EncodeToString(cipherText),
		CipherParams: cipherParamsJSON,
		KDF:          "scrypt",
		KDFParams:    scryptParamsJSON,
		MAC:          hex.EncodeToString(mac),
	}
	encryptedKeyJSON := encryptedKeyJSON{
		hex.EncodeToString(key.Address[:]),
		cryptoStruct,
		key.Id.String(),
		version,
	}
	keyJSON, err := json.Marshal(encryptedKeyJSON)
	if err != nil {
		return err
	}

	return writeKeyFile(key.Address, ks.keysDirPath, keyJSON)
}

func (ks keyStorePassphrase) DeleteKey(keyAddr common.Address, auth string) (err error) {
	// only delete if correct passphrase is given
	_, _, err = decryptKeyFromFile(ks.keysDirPath, keyAddr, auth)
	if err != nil {
		return err
	}

	return deleteKey(ks.keysDirPath, keyAddr)
}

func decryptKeyFromFile(keysDirPath string, keyAddr common.Address, auth string) (keyBytes []byte, keyId []byte, err error) {
	m := make(map[string]interface{})
	err = getKey(keysDirPath, keyAddr, &m)
	if err != nil {
		return
	}

	//v := reflect.ValueOf(m["version"])
	k := new(encryptedKeyJSON)
	err = getKey(keysDirPath, keyAddr, &k)
	if err != nil {
		return
	}
	return decryptKey(k, auth)
}

func decryptKey(keyProtected *encryptedKeyJSON, auth string) (keyBytes []byte, keyId []byte, err error) {
	if keyProtected.Version != version {
		return nil, nil, fmt.Errorf("Version not supported: %v", keyProtected.Version)
	}

	if keyProtected.Crypto.Cipher != "aes-128-ctr" {
		return nil, nil, fmt.Errorf("Cipher not supported: %v", keyProtected.Crypto.Cipher)
	}

	keyId = uuid.Parse(keyProtected.Id)
	mac, err := hex.DecodeString(keyProtected.Crypto.MAC)
	if err != nil {
		return nil, nil, err
	}

	iv, err := hex.DecodeString(keyProtected.Crypto.CipherParams.IV)
	if err != nil {
		return nil, nil, err
	}

	cipherText, err := hex.DecodeString(keyProtected.Crypto.CipherText)
	if err != nil {
		return nil, nil, err
	}

	derivedKey, err := getKDFKey(keyProtected.Crypto, auth)
	if err != nil {
		return nil, nil, err
	}

	calculatedMAC := Sha3(derivedKey[16:32], cipherText)
	if !bytes.Equal(calculatedMAC, mac) {
		return nil, nil, errors.New("Decryption failed: MAC mismatch")
	}

	plainText, err := aesCTRXOR(derivedKey[:16], cipherText, iv)
	if err != nil {
		return nil, nil, err
	}
	return plainText, keyId, err
}

func getKDFKey(cryptoJSON cryptoJSON, auth string) ([]byte, error) {
	authArray := []byte(auth)
	salt, err := hex.DecodeString(cryptoJSON.KDFParams["salt"].(string))
	if err != nil {
		return nil, err
	}
	dkLen := ensureInt(cryptoJSON.KDFParams["dklen"])

	if cryptoJSON.KDF == "scrypt" {
		n := ensureInt(cryptoJSON.KDFParams["n"])
		r := ensureInt(cryptoJSON.KDFParams["r"])
		p := ensureInt(cryptoJSON.KDFParams["p"])
		return scrypt.Key(authArray, salt, n, r, p, dkLen)

	} else if cryptoJSON.KDF == "pbkdf2" {
		c := ensureInt(cryptoJSON.KDFParams["c"])
		prf := cryptoJSON.KDFParams["prf"].(string)
		if prf != "hmac-sha256" {
			return nil, fmt.Errorf("Unsupported PBKDF2 PRF: ", prf)
		}
		key := pbkdf2.Key(authArray, salt, c, dkLen, sha256.New)
		return key, nil
	}

	return nil, fmt.Errorf("Unsupported KDF: ", cryptoJSON.KDF)
}

// TODO: can we do without this when unmarshalling dynamic JSON?
// why do integers in KDF params end up as float64 and not int after
// unmarshal?
func ensureInt(x interface{}) int {
	res, ok := x.(int)
	if !ok {
		res = int(x.(float64))
	}
	return res
}
