package cluster

import "fmt"

// NotLeaderError indicates the operation was rejected because this node is not the leader.
type NotLeaderError struct {
	Leader   string // leader Raft address
	LeaderID string // leader node ID
}

func (e *NotLeaderError) Error() string {
	if e.Leader != "" {
		return fmt.Sprintf("not leader, current leader is %s (%s)", e.LeaderID, e.Leader)
	}
	return "not leader, leader unknown"
}

// IsNotLeaderError checks if an error is a NotLeaderError.
func IsNotLeaderError(err error) (*NotLeaderError, bool) {
	if nle, ok := err.(*NotLeaderError); ok {
		return nle, true
	}
	return nil, false
}
