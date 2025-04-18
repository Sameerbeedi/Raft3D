package main

import (
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "os"
    "path/filepath"
    "strings"
    "time"

    "github.com/hashicorp/raft"
    raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

type Command struct {
    Op    string `json:"op,omitempty"`
    Key   string `json:"key,omitempty"`
    Value string `json:"value,omitempty"`
}

type KVStore struct {
    raft   *raft.Raft
    fsm    *FSM      // Store reference to the FSM
    nodeID raft.ServerID
}

type FSM struct {
    data map[string]string
}

type FSMSnapshot struct {
    data map[string]string
}

func (f *FSM) Apply(logEntry *raft.Log) interface{} {
    var cmd Command
    if err := json.Unmarshal(logEntry.Data, &cmd); err != nil {
        log.Printf("Failed to unmarshal command: %s", err)
        return nil
    }

    switch cmd.Op {
    case "set":
        f.data[cmd.Key] = cmd.Value
        return nil
    case "delete":
        delete(f.data, cmd.Key)
        return nil
    case "get": // Added to support consistent reads through Raft
        return f.data[cmd.Key]
    default:
        log.Printf("Unknown command op: %s", cmd.Op)
        return nil
    }
}

func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
    // Create a deep copy of the data for the snapshot
    dataCopy := make(map[string]string)
    for k, v := range f.data {
        dataCopy[k] = v
    }
    return &FSMSnapshot{data: dataCopy}, nil
}

func (f *FSM) Restore(rc io.ReadCloser) error {
    decoder := json.NewDecoder(rc)
    return decoder.Decode(&f.data)
}

func (f *FSMSnapshot) Persist(sink raft.SnapshotSink) error {
    err := func() error {
        b, err := json.Marshal(f.data)
        if err != nil {
            return err
        }

        if _, err := sink.Write(b); err != nil {
            return err
        }

        return sink.Close()
    }()

    if err != nil {
        sink.Cancel()
    }

    return err
}

func (f *FSMSnapshot) Release() {}

// NewKVStore creates a new key-value store backed by Raft
func NewKVStore(nodeID raft.ServerID, dataDir string, addr string, bootstrap bool) (*KVStore, error) {
    // Create the FSM
    fsm := &FSM{
        data: make(map[string]string),
    }

    // Create Raft configuration
    config := raft.DefaultConfig()
    config.LocalID = nodeID
    config.SnapshotInterval = 20 * time.Second
    config.SnapshotThreshold = 1024

    // Create log store and stable store using BoltDB
    boltDB, err := raftboltdb.NewBoltStore(filepath.Join(dataDir, "raft.db"))
    if err != nil {
        return nil, fmt.Errorf("new bolt store: %w", err)
    }

    // Create the log store and stable store
    logStore := boltDB
    stableStore := boltDB

    // Create the snapshot store
    snapshotStore, err := raft.NewFileSnapshotStore(dataDir, 3, os.Stderr)
    if err != nil {
        return nil, fmt.Errorf("new snapshot store: %w", err)
    }

    // Setup the transport
    tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
    if err != nil {
        return nil, fmt.Errorf("resolve tcp addr: %w", err)
    }

    transport, err := raft.NewTCPTransport(tcpAddr.String(), tcpAddr, 3, 10*time.Second, os.Stderr)
    if err != nil {
        return nil, fmt.Errorf("new tcp transport: %w", err)
    }

    // Create raft system
    r, err := raft.NewRaft(config, fsm, logStore, stableStore, snapshotStore, transport)
    if err != nil {
        return nil, fmt.Errorf("new raft: %w", err)
    }

    // Bootstrap if needed
    if bootstrap {
        configuration := raft.Configuration{
            Servers: []raft.Server{
                {
                    ID:      config.LocalID,
                    Address: transport.LocalAddr(),
                },
            },
        }
        r.BootstrapCluster(configuration)
    }

    return &KVStore{
        raft:   r,
        fsm:    fsm, // Store the FSM reference
        nodeID: nodeID,
    }, nil
}

// Get returns the value for the given key
func (k *KVStore) Get(key string) (string, error) {
    // For consistent reads, we can apply a Get command through Raft
    // This is optional - you can also read directly from the FSM's data map
    // if you're okay with potentially stale reads
    if k.raft.State() != raft.Leader {
        // Find leader and forward request
        leaderAddr := k.raft.Leader()
        if leaderAddr == "" {
            return "", errors.New("no leader available")
        }
        return "", fmt.Errorf("not leader, current leader is %s", leaderAddr)
    }

    // Direct read from FSM (may be slightly stale but much faster)
    val, exists := k.fsm.data[key]
    if !exists {
        return "", nil // Key not found
    }
    return val, nil
}

// Set sets the value for the given key
func (k *KVStore) Set(key, value string) error {
    if k.raft.State() != raft.Leader {
        return errors.New("not leader")
    }

    cmd := Command{
        Op:    "set",
        Key:   key,
        Value: value,
    }

    data, err := json.Marshal(cmd)
    if err != nil {
        return err
    }

    f := k.raft.Apply(data, 10*time.Second)
    return f.Error()
}

// Delete deletes the given key
func (k *KVStore) Delete(key string) error {
    if k.raft.State() != raft.Leader {
        return errors.New("not leader")
    }

    cmd := Command{
        Op:  "delete",
        Key: key,
    }

    data, err := json.Marshal(cmd)
    if err != nil {
        return err
    }

    f := k.raft.Apply(data, 10*time.Second)
    return f.Error()
}

// AddPeer adds a new node to the cluster
func (k *KVStore) AddPeer(nodeID, addr string) error {
    if k.raft.State() != raft.Leader {
        return errors.New("not leader")
    }

    configFuture := k.raft.GetConfiguration()
    if err := configFuture.Error(); err != nil {
        return err
    }

    serverID := raft.ServerID(nodeID)
    serverAddr := raft.ServerAddress(addr)
    
    for _, srv := range configFuture.Configuration().Servers {
        if srv.ID == serverID || srv.Address == serverAddr {
            if srv.ID == serverID && srv.Address == serverAddr {
                // Already a member
                return nil
            }
            
            future := k.raft.RemoveServer(serverID, 0, 0)
            if err := future.Error(); err != nil {
                return fmt.Errorf("error removing existing node %s: %w", nodeID, err)
            }
        }
    }

    future := k.raft.AddVoter(serverID, serverAddr, 0, 0)
    if err := future.Error(); err != nil {
        return fmt.Errorf("error adding node %s as %s: %w", nodeID, addr, err)
    }

    return nil
}

// RemovePeer removes a node from the cluster
func (k *KVStore) RemovePeer(nodeID string) error {
    if k.raft.State() != raft.Leader {
        return errors.New("not leader")
    }

    serverID := raft.ServerID(nodeID)
    future := k.raft.RemoveServer(serverID, 0, 0)
    if err := future.Error(); err != nil {
        return fmt.Errorf("error removing server %s: %w", nodeID, err)
    }

    return nil
}

// GetLeader returns the current leader's address
func (k *KVStore) GetLeader() string {
    return string(k.raft.Leader())
}

// IsLeader returns true if this node is the current leader
func (k *KVStore) IsLeader() bool {
    return k.raft.State() == raft.Leader
}

// HTTP server code
type HTTPServer struct {
    store *KVStore
}

func NewHTTPServer(addr string, store *KVStore) *http.Server {
    // Create a new HTTPServer with the store
    httpServer := &HTTPServer{
        store: store,
    }

    mux := http.NewServeMux()
    
    // Key-value operations
    mux.HandleFunc("/kv/", httpServer.handleKV)
    
    // Cluster management
    mux.HandleFunc("/join", httpServer.handleJoin)
    
    // Cluster status
    mux.HandleFunc("/status", httpServer.handleStatus)

    return &http.Server{
        Addr:    addr,
        Handler: mux,
    }
}

// Handler functions to implement the HTTP server methods
func (s *HTTPServer) handleKV(w http.ResponseWriter, r *http.Request) {
    key := strings.TrimPrefix(r.URL.Path, "/kv/")
    
    switch r.Method {
    case "GET":
        value, err := s.store.Get(key)
        if err != nil {
            if strings.Contains(err.Error(), "not leader") {
                http.Error(w, err.Error(), http.StatusTemporaryRedirect)
                return
            }
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.Write([]byte(value))
        
    case "PUT":
        body, err := io.ReadAll(r.Body)
        if err != nil {
            http.Error(w, err.Error(), http.StatusBadRequest)
            return
        }
        
        if err := s.store.Set(key, string(body)); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.WriteHeader(http.StatusOK)
        
    case "DELETE":
        if err := s.store.Delete(key); err != nil {
            http.Error(w, err.Error(), http.StatusInternalServerError)
            return
        }
        w.WriteHeader(http.StatusOK)
        
    default:
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
    }
}

func (s *HTTPServer) handleJoin(w http.ResponseWriter, r *http.Request) {
    if r.Method != "POST" {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }
    
    var req struct {
        NodeID string `json:"node_id"`
        Addr   string `json:"addr"`
    }
    
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    
    if err := s.store.AddPeer(req.NodeID, req.Addr); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    
    w.WriteHeader(http.StatusOK)
}

func (s *HTTPServer) handleStatus(w http.ResponseWriter, r *http.Request) {
    status := struct {
        Leader   string `json:"leader"`
        IsLeader bool   `json:"is_leader"`
        NodeID   string `json:"node_id"`
    }{
        Leader:   s.store.GetLeader(),
        IsLeader: s.store.IsLeader(),
        NodeID:   string(s.store.nodeID),
    }
    
    json.NewEncoder(w).Encode(status)
}

func main() {
    if len(os.Args) < 4 {
        fmt.Printf("Usage: %s <node-id> <raft-addr> <http-addr> [join-addr]\n", os.Args[0])
        fmt.Printf("Example: %s node1 127.0.0.1:7000 :8000\n", os.Args[0])
        fmt.Printf("Example: %s node2 127.0.0.1:7001 :8001 127.0.0.1:8000\n", os.Args[0])
        os.Exit(1)
    }
    
    nodeID := os.Args[1]
    raftAddr := os.Args[2]
    httpAddr := os.Args[3]
    
    // Determine if this node should bootstrap a new cluster
    bootstrap := len(os.Args) < 5 // No join address means bootstrap
    
    // Create data directory
    dataDir := fmt.Sprintf("./node_%s", nodeID)
    if err := os.MkdirAll(dataDir, 0755); err != nil {
        log.Fatalf("Failed to create data directory: %s", err)
    }
    
    // Create and start the KV store with Raft consensus
    log.Printf("Starting Raft node %s at %s", nodeID, raftAddr)
    store, err := NewKVStore(raft.ServerID(nodeID), dataDir, raftAddr, bootstrap)
    if err != nil {
        log.Fatalf("Failed to create KV store: %s", err)
    }
    
    // Start HTTP server
    httpServer := NewHTTPServer(httpAddr, store)
    log.Printf("Starting HTTP server on %s", httpAddr)
    
    // Join an existing cluster if specified
    if !bootstrap && len(os.Args) >= 5 {
        joinAddr := os.Args[4]
        log.Printf("Joining cluster at %s", joinAddr)
        
        // Wait a moment for the HTTP server in the leader to be ready
        time.Sleep(1 * time.Second)
        
        // Send a join request to the leader
        req, err := json.Marshal(map[string]string{
            "node_id": nodeID,
            "addr":    raftAddr,
        })
        if err != nil {
            log.Fatalf("Failed to marshal join request: %s", err)
        }
        
        resp, err := http.Post("http://"+joinAddr+"/join", "application/json", strings.NewReader(string(req)))
        if err != nil {
            log.Fatalf("Failed to join cluster: %s", err)
        }
        defer resp.Body.Close()
        
        if resp.StatusCode != http.StatusOK {
            body, _ := io.ReadAll(resp.Body)
            log.Fatalf("Failed to join cluster: %s", string(body))
        }
        
        log.Printf("Successfully joined cluster")
    }
    
    // Start serving HTTP requests
    log.Fatal(httpServer.ListenAndServe())
}