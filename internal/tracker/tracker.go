// Package tracker is the stateless coordination service for the peer network: a
// hash -> [nodes] content index plus node liveness. It knows nothing about genome
// structure (ADR 0003 §3). State is in-memory and rebuilds from node announces, so
// the tracker can restart without durable storage — nodes re-announce on their next
// heartbeat cycle.
package tracker

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luizeduardocarvalho/genomehub/internal/sign"
	"github.com/zeebo/blake3"
)

// DefaultTimeout is how long a node may go without a heartbeat before it is
// considered offline and dropped from peer lists.
const DefaultTimeout = 90 * time.Second

// announceSkew bounds how far an announce/leave timestamp may be from now, to
// reject replayed or clock-skewed signed requests.
const announceSkew = 5 * time.Minute

// CanonicalMessage is the exact byte string a signed announce/leave covers. It
// binds the operation, node identity (its public key), advertised address,
// freshness timestamp, and a digest of the held-hash set, so a captured
// signature can't be replayed as a different op or with a tampered hash list.
// Node and tracker compute it identically.
func CanonicalMessage(op, nodeID, address string, ts int64, hashesDigest string) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%d\n%s", op, nodeID, address, ts, hashesDigest))
}

// HashesDigest is a stable blake3 digest over a held-hash set (order-independent).
func HashesDigest(hashes []string) string {
	cp := append([]string(nil), hashes...)
	sort.Strings(cp)
	h := blake3.New()
	for _, x := range cp {
		h.WriteString(x)
		h.WriteString("\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

type nodeState struct {
	Address  string
	Kind     string
	LastSeen time.Time
	held     map[string]struct{} // hashes this node holds
}

// Registry is the in-memory tracker state.
type Registry struct {
	mu      sync.RWMutex
	nodes   map[string]*nodeState          // node id -> state
	content map[string]map[string]struct{} // hash -> set of node ids
	timeout time.Duration

	// RequireIdentity rejects unsigned announce/leave requests. When false,
	// unsigned requests are accepted (backward compatible) but any request that
	// IS signed is still verified.
	RequireIdentity bool

	// VerifyAnnounce makes the tracker spot-check that an announcing node
	// actually serves a random sample of the hashes it claims, by probing
	// HEAD /segments/{hash} at its advertised address. A node that announces
	// content it does not hold is rejected, so it can't draw fetch traffic it
	// will only 404.
	VerifyAnnounce bool
	verifySample   int
	httpClient     *http.Client
}

func NewRegistry(timeout time.Duration) *Registry {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Registry{
		nodes:        map[string]*nodeState{},
		content:      map[string]map[string]struct{}{},
		timeout:      timeout,
		verifySample: 3,
		httpClient:   &http.Client{Timeout: 3 * time.Second},
	}
}

// verifyHeld probes a random sample of the announced hashes against the node's
// advertised address (HEAD /segments/{hash}); any sampled hash the node does
// not serve fails the announce. Sampling keeps the cost bounded for nodes that
// hold millions of segments.
func (r *Registry) verifyHeld(a announceReq) error {
	addr := strings.TrimRight(a.Address, "/")
	for _, h := range sampleHashes(a.Hashes, r.verifySample) {
		req, err := http.NewRequest(http.MethodHead, addr+"/segments/"+strings.TrimPrefix(h, "blake3:"), nil)
		if err != nil {
			return err
		}
		resp, err := r.httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("probe %s: %w", h, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("node does not serve announced segment %s (status %d)", h, resp.StatusCode)
		}
	}
	return nil
}

// sampleHashes returns up to k distinct hashes chosen at random.
func sampleHashes(hashes []string, k int) []string {
	if len(hashes) <= k {
		return hashes
	}
	perm := rand.Perm(len(hashes))[:k]
	out := make([]string, k)
	for i, j := range perm {
		out[i] = hashes[j]
	}
	return out
}

type announceReq struct {
	NodeID    string   `json:"node_id"`
	Address   string   `json:"address"`
	Kind      string   `json:"kind"`
	Hashes    []string `json:"hashes"`
	Timestamp int64    `json:"ts,omitempty"`
	Signature string   `json:"sig,omitempty"` // hex ed25519 sig over CanonicalMessage; NodeID is the public key
}

// verify authenticates a signed request. NodeID is the signer's public key, so
// a valid signature proves the caller owns that identity — no node can announce
// or leave as another. An unsigned request is allowed unless RequireIdentity.
func (r *Registry) verify(op string, a announceReq) error {
	if a.Signature == "" {
		if r.RequireIdentity {
			return fmt.Errorf("unsigned request rejected (tracker requires identity)")
		}
		return nil
	}
	if d := time.Since(time.Unix(a.Timestamp, 0)); d > announceSkew || d < -announceSkew {
		return fmt.Errorf("timestamp outside acceptable window")
	}
	sig, err := hex.DecodeString(a.Signature)
	if err != nil {
		return fmt.Errorf("bad signature encoding")
	}
	msg := CanonicalMessage(op, a.NodeID, a.Address, a.Timestamp, HashesDigest(a.Hashes))
	ok, err := sign.Verify(a.NodeID, msg, sig)
	if err != nil {
		return fmt.Errorf("verify identity: %w", err)
	}
	if !ok {
		return fmt.Errorf("signature does not match node identity")
	}
	return nil
}

// announce registers (or refreshes) a node and replaces its held-hash set.
func (r *Registry) announce(a announceReq) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(a.NodeID) // clear any prior content entries before re-adding
	held := make(map[string]struct{}, len(a.Hashes))
	for _, h := range a.Hashes {
		held[h] = struct{}{}
		if r.content[h] == nil {
			r.content[h] = map[string]struct{}{}
		}
		r.content[h][a.NodeID] = struct{}{}
	}
	r.nodes[a.NodeID] = &nodeState{Address: a.Address, Kind: a.Kind, LastSeen: time.Now(), held: held}
}

// heartbeat refreshes a node's liveness; returns false if the node is unknown
// (so the caller re-announces).
func (r *Registry) heartbeat(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[nodeID]
	if !ok {
		return false
	}
	n.LastSeen = time.Now()
	return true
}

func (r *Registry) leave(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeLocked(nodeID)
}

// removeLocked deletes a node and all its content-index entries. Caller holds mu.
func (r *Registry) removeLocked(nodeID string) {
	n, ok := r.nodes[nodeID]
	if !ok {
		return
	}
	for h := range n.held {
		if set := r.content[h]; set != nil {
			delete(set, nodeID)
			if len(set) == 0 {
				delete(r.content, h)
			}
		}
	}
	delete(r.nodes, nodeID)
}

// peers returns the addresses of live nodes holding hash.
func (r *Registry) peers(hash string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for id := range r.content[hash] {
		if n, ok := r.nodes[id]; ok && time.Since(n.LastSeen) <= r.timeout {
			out = append(out, n.Address)
		}
	}
	sort.Strings(out)
	return out
}

// NodeView is the public per-node status used by /nodes (and dashboards).
type NodeView struct {
	NodeID     string `json:"node_id"`
	Address    string `json:"address"`
	Kind       string `json:"kind"`
	Held       int    `json:"held"`
	AgeSeconds int    `json:"age_seconds"`
	Online     bool   `json:"online"`
}

func (r *Registry) nodeViews() []NodeView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []NodeView
	for id, n := range r.nodes {
		age := time.Since(n.LastSeen)
		out = append(out, NodeView{
			NodeID:     id,
			Address:    n.Address,
			Kind:       n.Kind,
			Held:       len(n.held),
			AgeSeconds: int(age.Seconds()),
			Online:     age <= r.timeout,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// GC drops nodes whose heartbeat has timed out. Run periodically.
func (r *Registry) GC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, n := range r.nodes {
		if time.Since(n.LastSeen) > r.timeout {
			r.removeLocked(id)
		}
	}
}

// Handler builds the tracker HTTP API and starts a background GC loop.
func (r *Registry) Handler() http.Handler {
	go func() {
		t := time.NewTicker(r.timeout / 2)
		for range t.C {
			r.GC()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	mux.HandleFunc("POST /announce", func(w http.ResponseWriter, req *http.Request) {
		var a announceReq
		if err := json.NewDecoder(req.Body).Decode(&a); err != nil || a.NodeID == "" || a.Address == "" {
			http.Error(w, "bad announce", http.StatusBadRequest)
			return
		}
		if err := r.verify("announce", a); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if r.VerifyAnnounce && len(a.Hashes) > 0 {
			if err := r.verifyHeld(a); err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}
		}
		r.announce(a)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, req *http.Request) {
		var a announceReq
		if err := json.NewDecoder(req.Body).Decode(&a); err != nil || a.NodeID == "" {
			http.Error(w, "bad heartbeat", http.StatusBadRequest)
			return
		}
		if !r.heartbeat(a.NodeID) {
			http.Error(w, "unknown node, re-announce", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /leave", func(w http.ResponseWriter, req *http.Request) {
		var a announceReq
		if err := json.NewDecoder(req.Body).Decode(&a); err != nil || a.NodeID == "" {
			http.Error(w, "bad leave", http.StatusBadRequest)
			return
		}
		if err := r.verify("leave", a); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		r.leave(a.NodeID)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /peers/{hash}", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, map[string][]string{"peers": r.peers(req.PathValue("hash"))})
	})

	mux.HandleFunc("GET /nodes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, r.nodeViews())
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
