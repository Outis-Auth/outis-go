package outis

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// IntentParamKey is the params key that carries an intent's digest, so the
// operation hash (and the operators' approval) binds the exact call.
const IntentParamKey = "intent"

const (
	intentVersion = 1
	intentAlg     = "A256GCM"
	intentAAD     = "outis.intent.v1\x00"
	intentKeySize = 32
)

// Intent is the call a worker replays once a request is authorized: which
// registered client, which dotted method on it, and the JSON arguments.
type Intent struct {
	V      int               `json:"v"`
	Client string            `json:"client"`
	Method string            `json:"method"`
	Args   []json.RawMessage `json:"args"`
}

// IntentEnvelope is an intent sealed under the customer's key, as Outis
// stores and returns it. Outis can neither read nor alter it.
type IntentEnvelope struct {
	V          int    `json:"v"`
	Alg        string `json:"alg"`
	KeyID      string `json:"kid"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// EncodeIntent is the plaintext of an intent: compact UTF-8 JSON, produced
// once by the proposer and never re-serialized. It refuses args that aren't
// plain JSON data (funcs, channels, NaN and the like).
func EncodeIntent(client, method string, args []any) ([]byte, error) {
	if client == "" || method == "" {
		return nil, errors.New("outis: an intent needs a client and a method")
	}
	raw := make([]json.RawMessage, len(args))
	for i, a := range args {
		b, err := marshalCompact(a)
		if err != nil {
			return nil, fmt.Errorf("outis: intent arg %d isn't plain JSON data: %w", i, err)
		}
		raw[i] = b
	}
	return marshalCompact(Intent{V: intentVersion, Client: client, Method: method, Args: raw})
}

// marshalCompact is json.Marshal without HTML escaping, matching what the
// other Outis SDKs produce for the same values.
func marshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// IntentDigest is "sha256:" and the hex SHA-256 of an intent's plaintext.
func IntentDigest(plaintext []byte) string {
	sum := sha256.Sum256(plaintext)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IntentKeyID is a key's kid: the first 16 hex characters of its SHA-256.
func IntentKeyID(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:16]
}

// ParseIntentKey decodes OUTIS_INTENT_KEY: base64, standard or URL alphabet,
// padded or not, of exactly 32 bytes.
func ParseIntentKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != intentKeySize {
				return nil, fmt.Errorf("outis: an intent key is 32 bytes, this one is %d", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("outis: an intent key is base64 of 32 random bytes")
}

// ParseIntentKeys decodes a comma separated list, like OUTIS_INTENT_KEYS.
func ParseIntentKeys(s string) ([][]byte, error) {
	var keys [][]byte
	for _, part := range strings.Split(s, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		k, err := ParseIntentKey(part)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, errors.New("outis: no intent keys given")
	}
	return keys, nil
}

// GenerateIntentKey returns a new random key, base64 encoded for OUTIS_INTENT_KEY.
func GenerateIntentKey() (string, error) {
	b := make([]byte, intentKeySize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// SealIntent encrypts plaintext under key with AES-256-GCM and a random
// nonce, bound to action.
func SealIntent(key []byte, action string, plaintext []byte) (*IntentEnvelope, error) {
	return sealIntent(key, action, plaintext, rand.Reader)
}

func sealIntent(key []byte, action string, plaintext []byte, nonces io.Reader) (*IntentEnvelope, error) {
	aead, err := intentAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(nonces, nonce); err != nil {
		return nil, fmt.Errorf("outis: intent nonce: %w", err)
	}
	ct := aead.Seal(nil, nonce, plaintext, []byte(intentAAD+action))
	return &IntentEnvelope{
		V:          intentVersion,
		Alg:        intentAlg,
		KeyID:      IntentKeyID(key),
		Nonce:      base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawURLEncoding.EncodeToString(ct),
	}, nil
}

// OpenIntent decrypts env with whichever of keys matches its kid, for the
// request's action. It returns the plaintext exactly as the proposer
// produced it; a failure is an [*IntentError].
func OpenIntent(keys [][]byte, action string, env *IntentEnvelope) ([]byte, error) {
	if env == nil {
		return nil, &IntentError{Reason: ReasonNoIntent}
	}
	if env.V != intentVersion || env.Alg != intentAlg {
		return nil, &IntentError{Reason: ReasonBadIntent, Err: fmt.Errorf("unsupported envelope v%d %s", env.V, env.Alg)}
	}
	var key []byte
	for _, k := range keys {
		if IntentKeyID(k) == env.KeyID {
			key = k
			break
		}
	}
	if key == nil {
		return nil, &IntentError{Reason: ReasonUnknownKey, Err: fmt.Errorf("no key with kid %s", env.KeyID)}
	}
	nonce, err1 := decodeB64URL(env.Nonce)
	ct, err2 := decodeB64URL(env.Ciphertext)
	if err := errors.Join(err1, err2); err != nil {
		return nil, &IntentError{Reason: ReasonBadIntent, Err: err}
	}
	aead, err := intentAEAD(key)
	if err != nil {
		return nil, &IntentError{Reason: ReasonUnknownKey, Err: err}
	}
	if len(nonce) != aead.NonceSize() {
		return nil, &IntentError{Reason: ReasonBadIntent, Err: errors.New("nonce isn't 12 bytes")}
	}
	pt, err := aead.Open(nil, nonce, ct, []byte(intentAAD+action))
	if err != nil {
		return nil, &IntentError{Reason: ReasonDecryptFailed, Err: err}
	}
	return pt, nil
}

// DecodeIntent parses an intent's plaintext strictly. It refuses a field it
// doesn't know and a non-empty kwargs, since Go calls take no keyword
// arguments and dropping them would replay a different call.
func DecodeIntent(plaintext []byte) (*Intent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(plaintext, &fields); err != nil {
		return nil, &IntentError{Reason: ReasonBadIntent, Err: err}
	}
	for name, raw := range fields {
		switch name {
		case "v", "client", "method", "args":
		case "kwargs":
			var kw map[string]json.RawMessage
			if err := json.Unmarshal(raw, &kw); err != nil {
				return nil, &IntentError{Reason: ReasonBadIntent, Err: errors.New("kwargs isn't an object")}
			}
			if len(kw) > 0 {
				return nil, &IntentError{Reason: ReasonBadIntent, Err: errors.New("kwargs not supported")}
			}
		default:
			return nil, &IntentError{Reason: ReasonBadIntent, Err: errors.New("unknown field " + name)}
		}
	}
	var in Intent
	if err := json.Unmarshal(plaintext, &in); err != nil {
		return nil, &IntentError{Reason: ReasonBadIntent, Err: err}
	}
	if in.V != intentVersion || in.Client == "" || in.Method == "" {
		return nil, &IntentError{Reason: ReasonBadIntent, Err: errors.New("intent needs v 1, a client and a method")}
	}
	return &in, nil
}

func intentAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != intentKeySize {
		return nil, fmt.Errorf("outis: an intent key is 32 bytes, this one is %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func decodeB64URL(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// The reason codes a worker reports when it refuses or fails an intent.
const (
	ReasonNoIntent          = "no_intent"
	ReasonBadIntent         = "bad_intent"
	ReasonUnknownKey        = "unknown_key"
	ReasonDecryptFailed     = "decrypt_failed"
	ReasonDigestMismatch    = "digest_mismatch"
	ReasonOperationMismatch = "operation_mismatch"
	ReasonNotAuthorized     = "not_authorized"
	ReasonNotRegistered     = "client_not_registered"
	ReasonNotAllowed        = "not_allowed"
	ReasonBadArgs           = "bad_args"
	ReasonExecutionError    = "execution_error"
)

// IntentError is why a worker refused or failed an intent. Reason is one of
// the Reason codes, which is also how the report to Outis starts.
type IntentError struct {
	Reason string
	Err    error
}

func (e *IntentError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *IntentError) Unwrap() error { return e.Err }
