package approval

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strings"
)

// This file reads OpenSSH's own formats with the standard library alone:
// the public key wire blob of an authorized_keys line, and the SSHSIG
// signature `ssh-keygen -Y sign` writes (OpenSSH's PROTOCOL.sshsig, and
// PROTOCOL.u2f for the security-key types). The engine only verifies; it
// never holds or reads a private key.

// Namespace is the SSHSIG namespace an approval is signed under, so a
// signature made for git, for files or for any other purpose answers no
// challenge, and an approval signature answers nothing else:
//
//	ssh-keygen -Y sign -n brigada-approval -f <key> <challenge-file>
const Namespace = "brigada-approval"

// The key types an approval key may be. ssh-rsa is the RSA key type; its
// signatures must be rsa-sha2-256 or rsa-sha2-512 — SSHSIG refuses SHA-1.
const (
	typeEd25519   = "ssh-ed25519"
	typeSKEd25519 = "sk-ssh-ed25519@openssh.com"
	typeECDSA256  = "ecdsa-sha2-nistp256"
	typeECDSA384  = "ecdsa-sha2-nistp384"
	typeECDSA521  = "ecdsa-sha2-nistp521"
	typeSKECDSA   = "sk-ecdsa-sha2-nistp256@openssh.com"
	typeRSA       = "ssh-rsa"
)

// minRSABits is the smallest RSA modulus enrollment accepts.
const minRSABits = 2048

// skUserPresent is the security-key flag that says the user touched the
// key for this signature (PROTOCOL.u2f); skExtensions says extension data
// follows, which no approval signature carries.
const (
	skUserPresent = 0x01
	skExtensions  = 0x80
)

// wire reads the SSH wire encoding (RFC 4251 §5): big-endian uint32,
// length-prefixed strings. The first short read poisons the reader.
type wire struct {
	b   []byte
	bad bool
}

func (w *wire) u32() uint32 {
	if w.bad || len(w.b) < 4 {
		w.bad = true
		return 0
	}
	v := binary.BigEndian.Uint32(w.b)
	w.b = w.b[4:]
	return v
}

func (w *wire) byte1() byte {
	if w.bad || len(w.b) < 1 {
		w.bad = true
		return 0
	}
	v := w.b[0]
	w.b = w.b[1:]
	return v
}

func (w *wire) str() []byte {
	n := w.u32()
	if w.bad || uint64(n) > uint64(len(w.b)) {
		w.bad = true
		return nil
	}
	v := w.b[:n]
	w.b = w.b[n:]
	return v
}

// mpint reads a non-negative multiple-precision integer.
func (w *wire) mpint() *big.Int {
	v := w.str()
	if len(v) > 0 && v[0]&0x80 != 0 {
		w.bad = true // negative: no key or signature value is
	}
	return new(big.Int).SetBytes(v)
}

// done reports whether the reader consumed its input exactly.
func (w *wire) done() bool { return !w.bad && len(w.b) == 0 }

func putString(b *bytes.Buffer, s []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.Write(s)
}

// publicKey is a parsed approval key: its type, its wire blob (what
// fingerprints and the SSHSIG blob compare), and what verification needs.
type publicKey struct {
	typ  string
	blob []byte
	ed   ed25519.PublicKey
	ec   *ecdsa.PublicKey
	rsa  *rsa.PublicKey
	app  []byte // a security key's application, "ssh:" by default
}

// parsePublicKey reads a public key wire blob of a supported type and
// refuses everything else, so an unusable key fails at enrollment, not at
// the first approval.
func parsePublicKey(blob []byte) (*publicKey, error) {
	w := &wire{b: blob}
	typ := string(w.str())
	if w.bad {
		return nil, errors.New("not an SSH public key")
	}
	k := &publicKey{typ: typ, blob: bytes.Clone(blob)}
	switch typ {
	case typeEd25519, typeSKEd25519:
		key := w.str()
		if typ == typeSKEd25519 {
			k.app = bytes.Clone(w.str())
		}
		if !w.done() || len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("malformed %s key", typ)
		}
		k.ed = ed25519.PublicKey(bytes.Clone(key))
	case typeECDSA256, typeECDSA384, typeECDSA521, typeSKECDSA:
		curveName := string(w.str())
		point := w.str()
		if typ == typeSKECDSA {
			k.app = bytes.Clone(w.str())
		}
		if !w.done() {
			return nil, fmt.Errorf("malformed %s key", typ)
		}
		curve, want := curveOf(typ)
		if curveName != want {
			return nil, fmt.Errorf("%s key names curve %q, want %q", typ, curveName, want)
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(curve, point)
		if err != nil {
			return nil, fmt.Errorf("malformed %s key: %v", typ, err)
		}
		k.ec = pub
	case typeRSA:
		e := w.mpint()
		n := w.mpint()
		if !w.done() || !e.IsInt64() || e.Int64() < 3 || e.Int64()%2 == 0 || e.Int64() > 1<<31-1 {
			return nil, fmt.Errorf("malformed %s key", typ)
		}
		if n.BitLen() < minRSABits {
			return nil, fmt.Errorf("%s key of %d bits is too short; want at least %d", typ, n.BitLen(), minRSABits)
		}
		k.rsa = &rsa.PublicKey{N: n, E: int(e.Int64())}
	default:
		return nil, fmt.Errorf("unsupported key type %q; want %s", typ, strings.Join(supportedTypes, ", "))
	}
	return k, nil
}

// supportedTypes names the key types parsePublicKey accepts, for errors.
var supportedTypes = []string{typeEd25519, typeSKEd25519, typeECDSA256, typeECDSA384, typeECDSA521, typeSKECDSA, typeRSA}

func curveOf(typ string) (elliptic.Curve, string) {
	switch typ {
	case typeECDSA384:
		return elliptic.P384(), "nistp384"
	case typeECDSA521:
		return elliptic.P521(), "nistp521"
	default:
		return elliptic.P256(), "nistp256"
	}
}

// fingerprint is the key's OpenSSH SHA256 fingerprint, as `ssh-keygen -l`
// prints it.
func fingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// sshsig is a parsed SSHSIG blob (PROTOCOL.sshsig).
type sshsig struct {
	raw       []byte // the decoded blob, whole
	publicKey []byte
	namespace string
	reserved  []byte
	hashAlg   string
	signature []byte // the SSH signature: format name, blob and, for a security key, flags and counter
}

const (
	sigMagic   = "SSHSIG"
	sigVersion = 1
	armorBegin = "-----BEGIN SSH SIGNATURE-----"
	armorEnd   = "-----END SSH SIGNATURE-----"
)

// parseArmored reads the armored signature `ssh-keygen -Y sign` writes.
func parseArmored(text string) (*sshsig, error) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, armorBegin) || !strings.HasSuffix(text, armorEnd) {
		return nil, errors.New("not an armored SSH signature (want the -----BEGIN SSH SIGNATURE----- block ssh-keygen -Y sign writes)")
	}
	body := strings.Join(strings.Fields(text[len(armorBegin):len(text)-len(armorEnd)]), "")
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("the signature's base64 does not decode: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte(sigMagic)) {
		return nil, errors.New("the signature is not an SSHSIG blob")
	}
	w := &wire{b: raw[len(sigMagic):]}
	version := w.u32()
	s := &sshsig{
		raw:       raw,
		publicKey: w.str(),
		namespace: string(w.str()),
		reserved:  w.str(),
		hashAlg:   string(w.str()),
		signature: w.str(),
	}
	if !w.done() {
		return nil, errors.New("the SSHSIG blob is malformed")
	}
	if version != sigVersion {
		return nil, fmt.Errorf("SSHSIG version %d; want %d", version, sigVersion)
	}
	return s, nil
}

// signedData is what the key signs for message under SSHSIG: the magic,
// the namespace, the reserved field, the hash algorithm and the message's
// hash.
func signedData(namespace string, reserved []byte, hashAlg string, message []byte) ([]byte, error) {
	var h hash.Hash
	switch hashAlg {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return nil, fmt.Errorf("SSHSIG hash algorithm %q; want sha256 or sha512", hashAlg)
	}
	h.Write(message)
	var b bytes.Buffer
	b.WriteString(sigMagic)
	putString(&b, []byte(namespace))
	putString(&b, reserved)
	putString(&b, []byte(hashAlg))
	putString(&b, h.Sum(nil))
	return b.Bytes(), nil
}

// verify checks that sig is a signature by k over data, the SSHSIG signed
// data. A security key's signature must carry the user-presence flag: a
// touch is what puts the approval out of reach of a process at the
// keyboard's user.
func (k *publicKey) verify(data, sig []byte) error {
	w := &wire{b: sig}
	format := string(w.str())
	blob := w.str()
	var flags byte
	var counter uint32
	if k.app != nil {
		flags = w.byte1()
		counter = w.u32()
	}
	if !w.done() {
		return errors.New("the signature is malformed")
	}
	switch k.typ {
	case typeEd25519:
		if format != typeEd25519 || !ed25519.Verify(k.ed, data, blob) {
			return errors.New("the signature does not verify")
		}
	case typeSKEd25519, typeSKECDSA:
		if format != k.typ {
			return errors.New("the signature does not verify")
		}
		if flags&skUserPresent == 0 {
			return errors.New("the security key signed without user presence; sign again and touch the key")
		}
		if flags&skExtensions != 0 {
			return errors.New("the security key's signature carries extensions, which an approval does not take")
		}
		app := sha256.Sum256(k.app)
		msg := sha256.Sum256(data)
		var signed bytes.Buffer
		signed.Write(app[:])
		signed.WriteByte(flags)
		binary.Write(&signed, binary.BigEndian, counter)
		signed.Write(msg[:])
		if k.typ == typeSKEd25519 {
			if !ed25519.Verify(k.ed, signed.Bytes(), blob) {
				return errors.New("the signature does not verify")
			}
			return nil
		}
		digest := sha256.Sum256(signed.Bytes())
		return verifyECDSA(k.ec, digest[:], blob)
	case typeECDSA256, typeECDSA384, typeECDSA521:
		if format != k.typ {
			return errors.New("the signature does not verify")
		}
		var h hash.Hash
		switch k.typ {
		case typeECDSA256:
			h = sha256.New()
		case typeECDSA384:
			h = sha512.New384()
		default:
			h = sha512.New()
		}
		h.Write(data)
		return verifyECDSA(k.ec, h.Sum(nil), blob)
	case typeRSA:
		var alg crypto.Hash
		var digest []byte
		switch format {
		case "rsa-sha2-256":
			sum := sha256.Sum256(data)
			alg, digest = crypto.SHA256, sum[:]
		case "rsa-sha2-512":
			sum := sha512.Sum512(data)
			alg, digest = crypto.SHA512, sum[:]
		default:
			return fmt.Errorf("RSA signature format %q; want rsa-sha2-256 or rsa-sha2-512", format)
		}
		if rsa.VerifyPKCS1v15(k.rsa, alg, digest, blob) != nil {
			return errors.New("the signature does not verify")
		}
	default:
		return fmt.Errorf("unsupported key type %q", k.typ)
	}
	return nil
}

// verifyECDSA checks an SSH ECDSA signature blob: mpint r, mpint s.
func verifyECDSA(pub *ecdsa.PublicKey, digest, blob []byte) error {
	w := &wire{b: blob}
	r := w.mpint()
	s := w.mpint()
	if !w.done() || !ecdsa.Verify(pub, digest, r, s) {
		return errors.New("the signature does not verify")
	}
	return nil
}
