package fanout

import (
	"crypto/rand"
	"encoding/hex"
)

// newMessageID generates a server-side message identifier.
//
// It is 16 random bytes rather than a sequence number because message IDs are
// exposed to clients and used as dedup keys in client storage; a guessable ID
// would let one user probe for another's messages by identifier.
func newMessageID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
