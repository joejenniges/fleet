// Package agentid gives agents memorable names and lets any fleet process find
// which agent it belongs to.
package agentid

import (
	"crypto/rand"
	"encoding/hex"
	"hash/fnv"
	"os"

	"github.com/joejenniges/fleet/internal/procs"
	"github.com/joejenniges/fleet/internal/store"
)

var adjectives = []string{
	"amber", "brisk", "cedar", "dusky", "ember", "frosty", "gilded", "hazel", "indigo", "jade",
	"keen", "lunar", "misty", "noble", "olive", "plucky", "quiet", "rusty", "sable", "tidal",
	"umber", "velvet", "willow", "zesty", "coral", "slate", "copper", "mossy", "silver", "sunny",
}

var nouns = []string{
	"falcon", "otter", "badger", "heron", "lynx", "marten", "osprey", "puffin", "quail", "raven",
	"salmon", "tapir", "viper", "walrus", "yak", "zebra", "bison", "crane", "dingo", "egret",
	"ferret", "gecko", "ibis", "jackal", "koala", "lemur", "moose", "newt", "orca", "panda",
}

// NameFor returns a deterministic two-word name for an agent ID.
func NameFor(id string) string {
	h := fnv.New64a()
	h.Write([]byte(id))
	v := h.Sum64()
	return adjectives[v%uint64(len(adjectives))] + "-" + nouns[(v/uint64(len(adjectives)))%uint64(len(nouns))]
}

// NewID returns a random ID for agents that do not supply a session ID.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Resolve finds the agent the current process belongs to:
//  1. FLEET_AGENT_ID, if set;
//  2. otherwise the nearest ancestor process registered as an agent.
//
// Returns nil (no error) when called by a human outside any agent.
func Resolve(st *store.Store) (*store.Agent, error) {
	if id := os.Getenv("FLEET_AGENT_ID"); id != "" {
		return st.GetAgent(id)
	}
	for _, a := range procs.Ancestors(int32(os.Getpid())) {
		ag, err := st.AgentByProcess(a.PID, a.Ctime)
		if err != nil {
			return nil, err
		}
		if ag != nil {
			return ag, nil
		}
	}
	return nil, nil
}
