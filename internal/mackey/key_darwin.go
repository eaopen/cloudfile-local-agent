//go:build darwin && cgo

// Package mackey keeps device private keys in the user's macOS Keychain.
package mackey

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>


static SecKeyRef cf_key(const char *alias, int create, int *status) {
 CFDataRef tag = CFDataCreate(NULL, (const UInt8 *)alias, strlen(alias));
 const void *qk[] = {kSecClass, kSecAttrApplicationTag, kSecAttrKeyType, kSecAttrKeyClass, kSecReturnRef};
 const void *qv[] = {kSecClassKey, tag, kSecAttrKeyTypeECSECPrimeRandom, kSecAttrKeyClassPrivate, kCFBooleanTrue};
 CFDictionaryRef query = CFDictionaryCreate(NULL, qk, qv, 5, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
 SecKeyRef key = NULL;
 *status = SecItemCopyMatching(query, (CFTypeRef *)&key);
 CFRelease(query);
 if (!create || *status != errSecItemNotFound) { CFRelease(tag); return key; }
 int bits = 256;
 CFNumberRef size = CFNumberCreate(NULL, kCFNumberIntType, &bits);
 const void *pk[] = {kSecAttrIsPermanent, kSecAttrApplicationTag, kSecAttrIsExtractable};
 const void *pv[] = {kCFBooleanTrue, tag, kCFBooleanFalse};
 CFDictionaryRef privateAttrs = CFDictionaryCreate(NULL, pk, pv, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
 const void *ak[] = {kSecAttrKeyType, kSecAttrKeySizeInBits, kSecPrivateKeyAttrs};
 const void *av[] = {kSecAttrKeyTypeECSECPrimeRandom, size, privateAttrs};
 CFDictionaryRef attrs = CFDictionaryCreate(NULL, ak, av, 3, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
 CFErrorRef error = NULL;
 key = SecKeyCreateRandomKey(attrs, &error);
 *status = key ? 0 : (error ? (int)CFErrorGetCode(error) : -1);
 if (error) CFRelease(error);
 CFRelease(attrs); CFRelease(privateAttrs); CFRelease(size); CFRelease(tag);
 return key;
}
static int cf_delete(const char *alias) {
 CFDataRef tag = CFDataCreate(NULL, (const UInt8 *)alias, strlen(alias));
 const void *keys[] = {kSecClass, kSecAttrApplicationTag, kSecAttrKeyType, kSecAttrKeyClass};
 const void *values[] = {kSecClassKey, tag, kSecAttrKeyTypeECSECPrimeRandom, kSecAttrKeyClassPrivate};
 CFDictionaryRef query = CFDictionaryCreate(NULL, keys, values, 4, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
 int status = SecItemDelete(query);
 CFRelease(query); CFRelease(tag);
 return status;
}
static CFDataRef cf_public(SecKeyRef key) {
 SecKeyRef pub = SecKeyCopyPublicKey(key);
 if (!pub) return NULL;
 CFDataRef data = SecKeyCopyExternalRepresentation(pub, NULL);
 CFRelease(pub);
 return data;
}
static CFDataRef cf_sign(SecKeyRef key, const void *digest) {
 CFDataRef input = CFDataCreate(NULL, digest, 32);
 CFDataRef signature = SecKeyCreateSignature(key, kSecKeyAlgorithmECDSASignatureDigestX962SHA256, input, NULL);
 CFRelease(input);
 return signature;
}
*/
import "C"

import (
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"unsafe"
)

var aliasPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Key struct{ ref C.SecKeyRef }

// Create is used only after a local user starts enrollment. Existing keys are
// reused; a missing confirmed key must be opened with Open, never regenerated.
func Create(alias string) (*Key, error) { return load(alias, true) }
func Open(alias string) (*Key, error)   { return load(alias, false) }
func load(alias string, create bool) (*Key, error) {
	if !aliasPattern.MatchString(alias) {
		return nil, errors.New("canonical device key alias required")
	}
	tag := C.CString("com.cloudfile.agent2.device." + alias)
	defer C.free(unsafe.Pointer(tag))
	var status C.int
	var mode C.int
	if create {
		mode = 1
	}
	key := C.cf_key(tag, mode, &status)
	if key == 0 || status != 0 {
		return nil, errors.New("macOS Keychain device key unavailable")
	}
	return &Key{ref: key}, nil
}
func (k *Key) Close() {
	if k.ref != 0 {
		C.CFRelease(C.CFTypeRef(k.ref))
		k.ref = 0
	}
}
func bytesFrom(data C.CFDataRef) ([]byte, error) {
	if data == 0 {
		return nil, errors.New("macOS Keychain operation failed")
	}
	defer C.CFRelease(C.CFTypeRef(data))
	return C.GoBytes(unsafe.Pointer(C.CFDataGetBytePtr(data)), C.int(C.CFDataGetLength(data))), nil
}
func (k *Key) PublicJWK() (map[string]string, error) {
	if k.ref == 0 {
		return nil, errors.New("closed device key")
	}
	raw, err := bytesFrom(C.cf_public(k.ref))
	if err != nil {
		return nil, err
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), raw)
	if x == nil || y == nil {
		return nil, errors.New("P-256 public key required")
	}
	return map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(x.FillBytes(make([]byte, 32))), "y": base64.RawURLEncoding.EncodeToString(y.FillBytes(make([]byte, 32)))}, nil
}
func (k *Key) Thumbprint() (string, error) {
	jwk, err := k.PublicJWK()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(jwk)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// SignDigest exposes no private material. Protocol validation belongs to the
// caller, which must bind the server challenge to a confirmed local pairing.
func (k *Key) SignDigest(digest [32]byte) (string, error) {
	if k.ref == 0 {
		return "", errors.New("closed device key")
	}
	der, err := bytesFrom(C.cf_sign(k.ref, unsafe.Pointer(&digest[0])))
	if err != nil {
		return "", err
	}
	var sig struct{ R, S *big.Int }
	rest, err := asn1.Unmarshal(der, &sig)
	if err != nil || len(rest) != 0 || sig.R == nil || sig.S == nil || sig.R.Sign() <= 0 || sig.S.Sign() <= 0 || sig.R.Cmp(elliptic.P256().Params().N) >= 0 || sig.S.Cmp(elliptic.P256().Params().N) >= 0 {
		return "", errors.New("invalid Keychain signature")
	}
	raw := make([]byte, 64)
	sig.R.FillBytes(raw[:32])
	sig.S.FillBytes(raw[32:])
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// Delete removes only one explicitly identified CloudFile device key. Removing
// a local key does not revoke the server device; that remains a browser action.
func Delete(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return errors.New("canonical device key alias required")
	}
	tag := C.CString("com.cloudfile.agent2.device." + alias)
	defer C.free(unsafe.Pointer(tag))
	status := C.cf_delete(tag)
	if status != 0 && status != C.errSecItemNotFound {
		return errors.New("device key removal failed")
	}
	return nil
}
