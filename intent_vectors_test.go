package outis

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// TestIntentVectors pins the shared vectors every Outis SDK must reproduce
// byte for byte.
func TestIntentVectors(t *testing.T) {
	raw, err := os.ReadFile("intent-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Key      string `json:"key"`
		KeyURL   string `json:"key_url"`
		KeyHex   string `json:"key_hex"`
		Kid      string `json:"kid"`
		NonceHex string `json:"nonce_hex"`
		Intents  []struct {
			Action        string            `json:"action"`
			AADHex        string            `json:"aad_hex"`
			Plaintext     string            `json:"plaintext"`
			Digest        string            `json:"digest"`
			Envelope      IntentEnvelope    `json:"envelope"`
			Params        map[string]string `json:"params"`
			OperationHash string            `json:"operation_hash"`
		} `json:"intents"`
		CallbackSecret []struct {
			APIKey    string `json:"api_key"`
			SecretHex string `json:"secret_hex"`
		} `json:"callback_secret"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	key, err := ParseIntentKey(v.Key)
	if err != nil {
		t.Fatal(err)
	}
	keyURL, err := ParseIntentKey(v.KeyURL)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(key) != v.KeyHex || !bytes.Equal(key, keyURL) {
		t.Fatalf("key decodes to %x", key)
	}
	if IntentKeyID(key) != v.Kid {
		t.Errorf("kid = %s, want %s", IntentKeyID(key), v.Kid)
	}
	nonce, _ := hex.DecodeString(v.NonceHex)
	if len(v.Intents) == 0 {
		t.Fatal("no intent vectors")
	}

	for _, c := range v.Intents {
		t.Run(c.Action, func(t *testing.T) {
			if got := hex.EncodeToString([]byte(intentAAD + c.Action)); got != c.AADHex {
				t.Errorf("aad = %s", got)
			}
			pt, err := OpenIntent([][]byte{key}, c.Action, &c.Envelope)
			if err != nil {
				t.Fatal(err)
			}
			if string(pt) != c.Plaintext {
				t.Errorf("plaintext = %s", pt)
			}
			if IntentDigest(pt) != c.Digest || c.Params[IntentParamKey] != c.Digest {
				t.Errorf("digest = %s", IntentDigest(pt))
			}
			if OperationHash(c.Action, c.Params) != c.OperationHash {
				t.Errorf("operation hash = %s", OperationHash(c.Action, c.Params))
			}
			env, err := sealIntent(key, c.Action, []byte(c.Plaintext), bytes.NewReader(nonce))
			if err != nil {
				t.Fatal(err)
			}
			if *env != c.Envelope {
				t.Errorf("sealed %+v, want %+v", *env, c.Envelope)
			}
			in, err := DecodeIntent(pt)
			if err != nil {
				t.Fatal(err)
			}
			args := make([]any, len(in.Args))
			for i, a := range in.Args {
				args[i] = a
			}
			again, err := EncodeIntent(in.Client, in.Method, args)
			if err != nil || string(again) != c.Plaintext {
				t.Errorf("re-encoded %s, %v", again, err)
			}
		})
	}

	for _, c := range v.CallbackSecret {
		if got := hex.EncodeToString(CallbackSecret(c.APIKey)); got != c.SecretHex {
			t.Errorf("CallbackSecret(%s) = %s", c.APIKey, got)
		}
	}
}
