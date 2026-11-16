package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
)

var ErrInvalidCiphertext = errors.New("encrypted value is invalid")

const (
	vaultVersion = byte(1)
	vaultMagic   = "SPV"
	maxKeyID     = 64
)

// Vault encrypts with its active key and decrypts with every configured key.
// Keep old keys configured until their stored values have been re-encrypted.
// A Vault is immutable after construction and safe for concurrent use.
type Vault struct {
	keys   map[string]cipher.AEAD
	active string
}

func NewVault(keys map[string][]byte, active string) (*Vault, error) {
	if len(keys) == 0 || !validKeyID(active) {
		return nil, errors.New("invalid encryption keyring")
	}
	v := &Vault{keys: make(map[string]cipher.AEAD, len(keys)), active: active}
	for id, key := range keys {
		if !validKeyID(id) || len(key) != 32 {
			return nil, errors.New("encryption keys must be AES-256 keys with valid identifiers")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, errors.New("cannot initialize encryption key")
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, errors.New("cannot initialize encryption key")
		}
		v.keys[id] = aead
	}
	if v.keys[active] == nil {
		return nil, errors.New("active encryption key is missing")
	}
	return v, nil
}

func validKeyID(id string) bool {
	if len(id) == 0 || len(id) > maxKeyID {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Seal creates a versioned envelope containing the key ID, a fresh random nonce,
// and authenticated ciphertext. Both the envelope header and caller AAD are
// authenticated; ciphertext cannot be moved to a different account or purpose.
func (v *Vault) Seal(plaintext []byte, aad string) ([]byte, error) {
	if v == nil || v.keys[v.active] == nil {
		return nil, errors.New("encryption vault is unavailable")
	}
	aead := v.keys[v.active]
	header := append([]byte(vaultMagic), vaultVersion, byte(len(v.active)))
	header = append(header, v.active...)
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("cannot generate encryption nonce")
	}
	blob := make([]byte, 0, len(header)+len(nonce)+len(plaintext)+aead.Overhead())
	blob = append(blob, header...)
	blob = append(blob, nonce...)
	return aead.Seal(blob, nonce, plaintext, vaultAAD(header, aad)), nil
}

func (v *Vault) Open(blob []byte, aad string) ([]byte, error) {
	if v == nil || len(blob) < 5 || string(blob[:3]) != vaultMagic || blob[3] != vaultVersion {
		return nil, ErrInvalidCiphertext
	}
	idLen := int(blob[4])
	if idLen < 1 || idLen > maxKeyID || len(blob) < 5+idLen {
		return nil, ErrInvalidCiphertext
	}
	headerLen := 5 + idLen
	aead := v.keys[string(blob[5:headerLen])]
	if aead == nil || len(blob) < headerLen+aead.NonceSize()+aead.Overhead() {
		return nil, ErrInvalidCiphertext
	}
	nonce := blob[headerLen : headerLen+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, blob[headerLen+aead.NonceSize():], vaultAAD(blob[:headerLen], aad))
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return plain, nil
}

func vaultAAD(header []byte, aad string) []byte {
	result := make([]byte, len(header)+8+len(aad))
	copy(result, header)
	binary.BigEndian.PutUint64(result[len(header):], uint64(len(aad)))
	copy(result[len(header)+8:], aad)
	return result
}
