package outis

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

const operationDomain = "outis.operation.v1\x00"

// OperationHash identifies an operation by its action and params, and matches
// the operation_hash Outis reports on every request. It covers what would
// run, not who asked or when.
func OperationHash(action string, params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte(operationDomain))
	// Each string is a 4 byte big-endian length, then its UTF-8 bytes.
	write := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	write(action)
	for _, k := range keys {
		write(k)
		write(params[k])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Operation is an action and its params: what the operators approve and what
// your code later runs.
type Operation struct {
	Action string
	Params map[string]string
}

// Hash is [OperationHash] of the operation.
func (o Operation) Hash() string { return OperationHash(o.Action, o.Params) }
