package libre3

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"math/big"
	"os"
)

// Keys is the emulator's own trust root, made up on first run and kept
// across sensors. It stands in for Abbott's: it signs every emulated
// sensor's certificate and the app credential the emulator hands out, so an
// app pairs with an emulated sensor only when it trusts this key. None of it
// is Abbott's key material.
type Keys struct {
	SigningKey string `json:"signingKey"` // P-256 scalar, hex
	AppKey     string `json:"appKey"`     // the app credential's P-256 scalar, hex
}

func LoadOrCreateKeys(path string) (*Keys, error) {
	if b, err := ioutil.ReadFile(path); err == nil {
		var k Keys
		if err := json.Unmarshal(b, &k); err != nil {
			return nil, err
		}
		return &k, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	k := &Keys{SigningKey: hex.EncodeToString(randomScalar()), AppKey: hex.EncodeToString(randomScalar())}
	b, _ := json.MarshalIndent(k, "", "  ")
	if err := ioutil.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func randomScalar() []byte {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return k.Bytes()
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func signingKey(scalar []byte) *ecdsa.PrivateKey {
	c := elliptic.P256()
	k := &ecdsa.PrivateKey{D: new(big.Int).SetBytes(scalar)}
	k.PublicKey.Curve = c
	k.PublicKey.X, k.PublicKey.Y = c.ScalarBaseMult(scalar)
	return k
}

// SigningPublicKey is the 65-byte point an app passes to RoundWhiteDiscKit as a
// trusted patch-signing key.
func (k *Keys) SigningPublicKey() []byte {
	p := signingKey(mustHex(k.SigningKey)).PublicKey
	return elliptic.Marshal(p.Curve, p.X, p.Y)
}

// sign is ECDSA-P256 over SHA-256, raw r || s: how both certificates are signed.
func (k *Keys) sign(msg []byte) []byte {
	h := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, signingKey(mustHex(k.SigningKey)), h[:])
	if err != nil {
		panic(err)
	}
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out
}

func (k *Keys) verify(msg, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	h := sha256.Sum256(msg)
	pub := signingKey(mustHex(k.SigningKey)).PublicKey
	return ecdsa.Verify(&pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func publicPoint(scalar []byte) []byte {
	k, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		panic(err)
	}
	return k.PublicKey().Bytes()
}

// AppCertificate is the 162-byte phone certificate of the app credential:
// version 03 03, a 16-byte pattern, a 15-byte header, the static public key
// and a signature over everything before it.
func (k *Keys) AppCertificate() []byte {
	c := make([]byte, 0, 162)
	c = append(c, 0x03, 0x03)
	for i := 0; i < 16; i++ {
		c = append(c, byte(i))
	}
	c = append(c, []byte("libre3sim app  ")...)
	c = append(c, publicPoint(mustHex(k.AppKey))...)
	return append(c, k.sign(c)...)
}

// AppCredentials is the credential file RoundWhiteDiscKit's
// AppIdentities.install(json:) takes.
func (k *Keys) AppCredentials() map[string]interface{} {
	return map[string]interface{}{
		"format": 1,
		"curve":  "P-256",
		"identities": []map[string]string{{
			"label":          "libre3sim",
			"privateKeyHex":  k.AppKey,
			"certificateHex": hex.EncodeToString(k.AppCertificate()),
		}},
	}
}

// acceptsPhoneCert is the sensor's check of the certificate an app loads: it
// must be signed by this trust root.
func (k *Keys) acceptsPhoneCert(cert []byte) bool {
	return len(cert) == 162 && cert[33] == 0x04 && k.verify(cert[:98], cert[98:])
}

// sensorCertificate is the 140-byte certificate a sensor sends: an 11-byte
// header, its static public key, and the trust root's signature over both.
func (k *Keys) sensorCertificate(serial string, staticPub []byte) []byte {
	c := []byte{0x01}
	h := sha256.Sum256([]byte(serial))
	c = append(c, h[:10]...)
	c = append(c, staticPub...)
	return append(c, k.sign(c)...)
}

func ecdhSecret(scalar, peer []byte) ([]byte, error) {
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		return nil, err
	}
	pub, err := ecdh.P256().NewPublicKey(peer)
	if err != nil {
		return nil, err
	}
	return priv.ECDH(pub)
}

// authKey is kAuth = SHA-256(00000001 || Ze || Zs)[:16], from the sensor's
// side: Ze = its ephemeral × the phone's ephemeral, Zs = its static × the
// app's static.
func authKey(sensorEph, phoneEph, sensorStatic, appStatic []byte) ([]byte, error) {
	ze, err := ecdhSecret(sensorEph, phoneEph)
	if err != nil {
		return nil, err
	}
	zs, err := ecdhSecret(sensorStatic, appStatic)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(append(append([]byte{0, 0, 0, 1}, ze...), zs...))
	return h[:16], nil
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
