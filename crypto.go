package sphinx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/aead/chacha20"
	secp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/ltcsuite/ltcd/btcec/v2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	// HMACSize is the length of the HMACs used to verify the integrity of
	// the onion. Any value lower than 32 will truncate the HMAC both
	// during onion creation as well as during the verification.
	HMACSize = 32
)

// chaChaPolyZeroNonce is a slice of zero bytes used in the chacha20poly1305
// encryption and decryption.
var chaChaPolyZeroNonce [chacha20poly1305.NonceSize]byte

// Hash256 is a statically sized, 32-byte array, typically containing
// the output of a SHA256 hash.
type Hash256 [sha256.Size]byte

// SingleKeyECDH is an abstraction interface that hides the implementation of an
// ECDH operation against a specific private key. We use this abstraction for
// the long term keys which we eventually want to be able to keep in a hardware
// wallet or HSM.
type SingleKeyECDH interface {
	// PubKey returns the public key of the private key that is abstracted
	// away by the interface.
	PubKey() *btcec.PublicKey

	// ECDH performs a scalar multiplication (ECDH-like operation) between
	// the abstracted private key and a remote public key. The output
	// returned will be the sha256 of the resulting shared point serialized
	// in compressed format.
	ECDH(pubKey *btcec.PublicKey) ([32]byte, error)
}

// PrivKeyECDH is an implementation of the SingleKeyECDH in which we do have the
// full private key. This can be used to wrap a temporary key to conform to the
// SingleKeyECDH interface.
type PrivKeyECDH struct {
	// PrivKey is the private key that is used for the ECDH operation.
	PrivKey *btcec.PrivateKey
}

// PubKey returns the public key of the private key that is abstracted away by
// the interface.
//
// NOTE: This is part of the SingleKeyECDH interface.
func (p *PrivKeyECDH) PubKey() *btcec.PublicKey {
	return p.PrivKey.PubKey()
}

// ECDH performs a scalar multiplication (ECDH-like operation) between the
// abstracted private key and a remote public key. The output returned will be
// the sha256 of the resulting shared point serialized in compressed format. If
// k is our private key, and P is the public key, we perform the following
// operation:
//
//	sx := k*P
//	s := sha256(sx.SerializeCompressed())
//
// NOTE: This is part of the SingleKeyECDH interface.
func (p *PrivKeyECDH) ECDH(pub *btcec.PublicKey) ([32]byte, error) {
	var pubJ btcec.JacobianPoint
	pub.AsJacobian(&pubJ)

	var ecdhPoint btcec.JacobianPoint
	btcec.ScalarMultNonConst(&p.PrivKey.Key, &pubJ, &ecdhPoint)

	ecdhPoint.ToAffine()
	ecdhPubKey := btcec.NewPublicKey(&ecdhPoint.X, &ecdhPoint.Y)

	return sha256.Sum256(ecdhPubKey.SerializeCompressed()), nil
}

// DecryptedError contains the decrypted error message and its sender.
type DecryptedError struct {
	// Sender is the node that sent the error. Note that a node may occur in
	// the path multiple times. If that is the case, the sender pubkey does
	// not tell the caller on which visit the error occurred.
	Sender *btcec.PublicKey

	// SenderIdx is the position of the error sending node in the path.
	// Index zero is the self node. SenderIdx allows to distinguish between
	// errors from nodes that occur in the path multiple times.
	SenderIdx int

	// Message is the decrypted error message.
	Message []byte
}

// zeroHMAC is the special HMAC value that allows the final node to determine
// if it is the payment destination or not.
var zeroHMAC [HMACSize]byte

// calcMac calculates HMAC-SHA-256 over the message using the passed secret key
// as input to the HMAC.
func calcMac(key [keyLen]byte, msg []byte) [HMACSize]byte {
	hmac := hmac.New(sha256.New, key[:])
	hmac.Write(msg)
	h := hmac.Sum(nil)

	var mac [HMACSize]byte
	copy(mac[:], h[:HMACSize])

	return mac
}

// xor computes the byte wise XOR of a and b, storing the result in dst. Only
// the frist `min(len(a), len(b))` bytes will be xor'd.
func xor(dst, a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dst[i] = a[i] ^ b[i]
	}
	return n
}

// generateKey generates a new key for usage in Sphinx packet
// construction/processing based off of the denoted keyType. Within Sphinx
// various keys are used within the same onion packet for padding generation,
// MAC generation, and encryption/decryption.
func generateKey(keyType string, sharedKey *Hash256) [keyLen]byte {
	mac := hmac.New(sha256.New, []byte(keyType))
	mac.Write(sharedKey[:])
	h := mac.Sum(nil)

	var key [keyLen]byte
	copy(key[:], h[:keyLen])

	return key
}

// generateCipherStream generates a stream of cryptographic psuedo-random bytes
// intended to be used to encrypt a message using a one-time-pad like
// construction.
func generateCipherStream(key [keyLen]byte, numBytes uint) []byte {
	var (
		nonce [8]byte
	)
	cipher, err := chacha20.NewCipher(nonce[:], key[:])
	if err != nil {
		panic(err)
	}
	output := make([]byte, numBytes)
	cipher.XORKeyStream(output, output)

	return output
}

// computeBlindingFactor for the next hop given the ephemeral pubKey and
// sharedSecret for this hop. The blinding factor is computed as the
// sha-256(pubkey || sharedSecret).
func computeBlindingFactor(hopPubKey *btcec.PublicKey,
	hopSharedSecret []byte) btcec.ModNScalar {

	sha := sha256.New()
	sha.Write(hopPubKey.SerializeCompressed())
	sha.Write(hopSharedSecret)

	var hash Hash256
	copy(hash[:], sha.Sum(nil))

	var blindingBytes btcec.ModNScalar
	blindingBytes.SetByteSlice(hash[:])

	return blindingBytes
}

// blindGroupElement blinds the group element P by performing scalar
// multiplication of the group element by blindingFactor: blindingFactor * P.
func blindGroupElement(hopPubKey *btcec.PublicKey, blindingFactor btcec.ModNScalar) *btcec.PublicKey {
	var hopPubKeyJ btcec.JacobianPoint
	hopPubKey.AsJacobian(&hopPubKeyJ)

	var blindedPoint btcec.JacobianPoint
	btcec.ScalarMultNonConst(
		&blindingFactor, &hopPubKeyJ, &blindedPoint,
	)
	blindedPoint.ToAffine()

	return btcec.NewPublicKey(&blindedPoint.X, &blindedPoint.Y)
}

// blindBaseElement blinds the groups's generator G by performing scalar base
// multiplication using the blindingFactor: blindingFactor * G.
func blindBaseElement(blindingFactor btcec.ModNScalar) *btcec.PublicKey {
	// TODO(roasbeef): remove after btcec version bump to add alias for
	// this method
	priv := secp.NewPrivateKey(&blindingFactor)
	return priv.PubKey()
}

// chacha20polyEncrypt initialises the ChaCha20Poly1305 algorithm with the given
// key and uses it to encrypt the passed message. This uses an all-zero nonce as
// required by the route-blinding spec.
func chacha20polyEncrypt(key, plainTxt []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}

	return aead.Seal(plainTxt[:0], chaChaPolyZeroNonce[:], plainTxt, nil),
		nil
}

// chacha20polyDecrypt initialises the ChaCha20Poly1305 algorithm with the given
// key and uses it to decrypt the passed cipher text. This uses an all-zero
// nonce as required by the route-blinding spec.
func chacha20polyDecrypt(key, cipherTxt []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}

	return aead.Open(cipherTxt[:0], chaChaPolyZeroNonce[:], cipherTxt, nil)
}

// sharedSecretGenerator is an interface that abstracts away exactly *how* the
// shared secret for each hop is generated.
//
// TODO(roasbef): rename?
type sharedSecretGenerator interface {
	// generateSharedSecret given a public key, generates a shared secret
	// using private data of the underlying sharedSecretGenerator.
	generateSharedSecret(dhKey *btcec.PublicKey) (Hash256, error)
}

// generateSharedSecret generates the shared secret using the given ephemeral
// pub key and the Router's private key. If a blindingPoint is provided then it
// is used to tweak the Router's private key before creating the shared secret
// with the ephemeral pub key. The blinding point is used to determine our
// shared secret with the receiver. From that we can determine our shared
// secret with the sender using the dhKey.
func (r *Router) generateSharedSecret(dhKey,
	blindingPoint *btcec.PublicKey) (Hash256, error) {

	// If no blinding point is provided, then the un-tweaked dhKey can
	// be used to derive the shared secret
	if blindingPoint == nil {
		return sharedSecret(r.onionKey, dhKey)
	}

	// We use the blinding point to calculate the blinding factor that the
	// receiver used with us so that we can use it to tweak our priv key.
	// The sender would have created their shared secret with our blinded
	// pub key.
	// 	* ss_receiver = H(k * E_receiver)
	ssReceiver, err := sharedSecret(r.onionKey, blindingPoint)
	if err != nil {
		return Hash256{}, err
	}

	// Compute the blinding factor that the receiver would have used to
	// blind our public key.
	//
	// 	* bf = HMAC256("blinded_node_id", ss_receiver)
	blindingFactorBytes := generateKey(routeBlindingHMACKey, &ssReceiver)
	var blindingFactor btcec.ModNScalar
	blindingFactor.SetBytes(&blindingFactorBytes)

	// Now, we want to calculate the shared secret between the sender and
	// our blinded key. In other words we want to calculate:
	// 	* ss_sender = H(E_sender * bf * k)
	//
	// Since the order in which the above multiplication happens does not
	// matter, we will first multiply E_sender with the blinding factor:
	blindedEphemeral := blindGroupElement(dhKey, blindingFactor)

	// Finally, we compute the ECDH to get the shared secret, ss_sender:
	return sharedSecret(r.onionKey, blindedEphemeral)
}

// sharedSecret does a ECDH operation on the passed private and public keys and
// returns the result.
func sharedSecret(priv SingleKeyECDH, pub *btcec.PublicKey) (Hash256, error) {
	var sharedSecret Hash256

	// Ensure that the public key is on our curve.
	if !pub.IsOnCurve() {
		return sharedSecret, ErrInvalidOnionKey
	}

	// Compute the shared secret.
	return priv.ECDH(pub)
}

// onionEncrypt obfuscates the data with compliance with BOLT#4. As we use a
// stream cipher, calling onionEncrypt on an already encrypted piece of data
// will decrypt it.
func onionEncrypt(sharedSecret *Hash256, data []byte) []byte {
	p := make([]byte, len(data))

	ammagKey := generateKey("ammag", sharedSecret)
	streamBytes := generateCipherStream(ammagKey, uint(len(data)))
	xor(p, data, streamBytes)

	return p
}

// minOnionErrorLength is the minimally expected length of the onion error
// message. Including padding, all messages on the wire should be at least 256
// bytes. We then add the size of the sha256 HMAC as well.
const minOnionErrorLength = 2 + 2 + 256 + sha256.Size

// DecryptError attempts to decrypt the passed encrypted error response. The
// onion failure is encrypted in backward manner, starting from the node where
// error have occurred. As a result, in order to decrypt the error we need get
// all shared secret and apply decryption in the reverse order. A structure is
// returned that contains the decrypted error message and information on the
// sender.
func (o *OnionErrorDecrypter) DecryptError(encryptedData []byte) (
	*DecryptedError, error) {

	// Ensure the error message length is as expected.
	if len(encryptedData) < minOnionErrorLength {
		return nil, fmt.Errorf("invalid error length: "+
			"expected at least %v got %v", minOnionErrorLength,
			len(encryptedData))
	}

	sharedSecrets, err := generateSharedSecrets(
		o.circuit.PaymentPath,
		o.circuit.SessionKey,
	)
	if err != nil {
		return nil, fmt.Errorf("error generating shared secret: %v",
			err)
	}

	var (
		sender      int
		msg         []byte
		dummySecret Hash256
	)
	copy(dummySecret[:], bytes.Repeat([]byte{1}, 32))

	// We'll iterate a constant amount of hops to ensure that we don't give
	// away an timing information pertaining to the position in the route
	// that the error emanated from.
	for i := 0; i < NumMaxHops; i++ {
		var sharedSecret Hash256

		// If we've already found the sender, then we'll use our dummy
		// secret to continue decryption attempts to fill out the rest
		// of the loop. Otherwise, we'll use the next shared secret in
		// line.
		if sender != 0 || i > len(sharedSecrets)-1 {
			sharedSecret = dummySecret
		} else {
			sharedSecret = sharedSecrets[i]
		}

		// With the shared secret, we'll now strip off a layer of
		// encryption from the encrypted error payload.
		encryptedData = onionEncrypt(&sharedSecret, encryptedData)

		// Next, we'll need to separate the data, from the MAC itself
		// so we can reconstruct and verify it.
		expectedMac := encryptedData[:sha256.Size]
		data := encryptedData[sha256.Size:]

		// With the data split, we'll now re-generate the MAC using its
		// specified key.
		umKey := generateKey("um", &sharedSecret)
		h := hmac.New(sha256.New, umKey[:])
		h.Write(data)

		// If the MAC matches up, then we've found the sender of the
		// error and have also obtained the fully decrypted message.
		realMac := h.Sum(nil)
		if hmac.Equal(realMac, expectedMac) && sender == 0 {
			sender = i + 1
			msg = data
		}
	}

	// If the sender index is still zero, then we haven't found the sender,
	// meaning we've failed to decrypt.
	if sender == 0 {
		return nil, errors.New("unable to retrieve onion failure")
	}

	return &DecryptedError{
		SenderIdx: sender,
		Sender:    o.circuit.PaymentPath[sender-1],
		Message:   msg,
	}, nil
}

// EncryptError is used to make data obfuscation using the generated shared
// secret.
//
// In context of Lightning Network is either used by the nodes in order to make
// initial obfuscation with the creation of the hmac or by the forwarding nodes
// for backward failure obfuscation of the onion failure blob. By obfuscating
// the onion failure on every node in the path we are adding additional step of
// the security and barrier for malware nodes to retrieve valuable information.
// The reason for using onion obfuscation is to not give
// away to the nodes in the payment path the information about the exact
// failure and its origin.
func (o *OnionErrorEncrypter) EncryptError(initial bool, data []byte) []byte {
	if initial {
		umKey := generateKey("um", &o.sharedSecret)
		hash := hmac.New(sha256.New, umKey[:])
		hash.Write(data)
		h := hash.Sum(nil)
		data = append(h, data...)
	}

	return onionEncrypt(&o.sharedSecret, data)
}
