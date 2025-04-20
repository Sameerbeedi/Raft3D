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
	"sync"

	"github.com/hashicorp/raft"
	"github.com/google/uuid"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

type PrintJobStatus string

const (
    StatusQueued   PrintJobStatus = "Queued"
    StatusRunning  PrintJobStatus = "Running"
    StatusComplete PrintJobStatus = "Complete"
    StatusFailed   PrintJobStatus = "Failed"
)

type PrintJob struct {
    ID                  string         `json:"id"`         // Unique ID for the print job
    PrinterID           string         `json:"printer_id"` // ID of the printer to use
    FilamentID          string         `json:"filament_id"`// ID of the filament to use
    PrintWeightInGrams  int            `json:"print_weight_in_grams"` // Estimated filament weight needed
    Status              PrintJobStatus `json:"status"`     // Current status (Queued, Running, etc.)
    CreatedAt           time.Time      `json:"created_at"` // Timestamp when created
    // Add other relevant fields like file_name, estimated_duration, etc. if needed
}


type Command struct {
	Op    string `json:"op,omitempty"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type KVStore struct {
	raft   *raft.Raft
	fsm    *FSM // Store reference to the FSM
	nodeID raft.ServerID
}

type FSM struct {
	data      map[string]string
	printers  map[string]Printer
	filaments map[string]Filament
	printJobs map[string]PrintJob
	mu sync.RWMutex
}

type Printer struct {
	ID      string `json:"id"`
	Company string `json:"company"`
	Model   string `json:"model"`
}

type Filament struct {
	ID                     string `json:"id"`
	Type                   string `json:"type"`
	Color                  string `json:"color"`
	TotalWeightInGrams     int    `json:"total_weight_in_grams"`
	RemainingWeightInGrams int    `json:"remaining_weight_in_grams"`
}

type FSMSnapshot struct {
	data map[string]string
}

// Modify FSM constructor to initialize printers map
func NewFSM() *FSM {
	return &FSM{
		data:      make(map[string]string),
		printers:  make(map[string]Printer),
		filaments: make(map[string]Filament),
		printJobs: make(map[string]PrintJob),
	}
}

func (f *FSM) Apply(logEntry *raft.Log) interface{} {
	f.mu.Lock() // Lock for write access
    	defer f.mu.Unlock()

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
	case "add_printer": // Handle adding a printer
		var printer Printer
		if err := json.Unmarshal([]byte(cmd.Value), &printer); err != nil {
			log.Printf("Failed to unmarshal printer: %s", err)
			return nil
		}
		f.printers[printer.ID] = printer
		return nil
	case "get_printers": // Handle retrieving all printers
		return f.printers
	case "add_filament": // Handle adding a filament
		var filament Filament
		if err := json.Unmarshal([]byte(cmd.Value), &filament); err != nil {
			log.Printf("Failed to unmarshal filament: %s", err)
			return nil
		}
		f.filaments[filament.ID] = filament
		return nil
	case "get_filaments": // Handle retrieving all filaments
		return f.filaments
	case "add_print_job": // Handle adding a print job
        var job PrintJob
        if err := json.Unmarshal([]byte(cmd.Value), &job); err != nil {
            log.Printf("Failed to unmarshal print job: %s", err)
            return err // Return error to Apply
        }
        // Update filament weight (this assumes validation passed before Apply)
        if filament, ok := f.filaments[job.FilamentID]; ok {
             // Note: In a real-world scenario, you might only *reserve* the weight
             // here and deduct it fully when the job status changes to Running.
             // For simplicity here, we deduct upfront.
            filament.RemainingWeightInGrams -= job.PrintWeightInGrams
            f.filaments[job.FilamentID] = filament
        } else {
             // This case should ideally not happen if validation works correctly before Apply
             log.Printf("CRITICAL: Filament %s not found during Apply for job %s", job.FilamentID, job.ID)
             return fmt.Errorf("filament %s not found during Apply", job.FilamentID)
        }
        f.printJobs[job.ID] = job
        log.Printf("Applied add_print_job for ID: %s", job.ID)
        return nil // Successfully applied

	default:
		log.Printf("Unknown command op: %s", cmd.Op)
		return nil
	}
	return nil
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

func NewKVStore(nodeID raft.ServerID, dataDir string, addr string, bootstrap bool) (*KVStore, error) {
	// Use NewFSM to properly initialize the FSM
	fsm := NewFSM()

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

	// Printer management
	mux.HandleFunc("/api/v1/printers", httpServer.handlePrinters)

	// Filament management
	mux.HandleFunc("/api/v1/filaments", httpServer.handleFilaments)

	mux.HandleFunc("/api/v1/print_jobs", httpServer.handlePrintJobs)

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

func (k *KVStore) AddPrinter(printer Printer) error {
	if k.raft.State() != raft.Leader {
		return errors.New("not leader")
	}

	// Check if the printer already exists
	if _, exists := k.fsm.printers[printer.ID]; exists {
		return fmt.Errorf("printer with ID %s already exists", printer.ID)
	}

	cmd := Command{
		Op:    "add_printer",
		Key:   printer.ID,
		Value: string(mustMarshal(printer)),
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	f := k.raft.Apply(data, 10*time.Second)
	return f.Error()
}

func (k *KVStore) GetPrinters() (map[string]Printer, error) {
	return k.fsm.printers, nil
}

// Helper function to marshal JSON
func mustMarshal(v interface{}) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func (s *HTTPServer) handlePrinters(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		var printer Printer
		if err := json.NewDecoder(r.Body).Decode(&printer); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := s.store.AddPrinter(printer); err != nil {
			if strings.Contains(err.Error(), "not leader") {
				http.Error(w, err.Error(), http.StatusTemporaryRedirect)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)

	case "GET":
		// Directly read from the FSM's local state
		printers, err := s.store.GetPrinters()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		json.NewEncoder(w).Encode(printers)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (k *KVStore) AddFilament(filament Filament) error {
	if k.raft.State() != raft.Leader {
		return errors.New("not leader")
	}

	// Check if the filament already exists
	if _, exists := k.fsm.filaments[filament.ID]; exists {
		return fmt.Errorf("filament with ID %s already exists", filament.ID)
	}

	cmd := Command{
		Op:    "add_filament",
		Key:   filament.ID,
		Value: string(mustMarshal(filament)),
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}

	f := k.raft.Apply(data, 10*time.Second)
	return f.Error()
}

func (k *KVStore) GetFilaments() (map[string]Filament, error) {
	// Allow followers to serve read requests
	return k.fsm.filaments, nil
}

// AddPrintJob creates and validates a new print job request
func (k *KVStore) AddPrintJob(reqJob PrintJob) (*PrintJob, error) {
    if k.raft.State() != raft.Leader {
        // Return error or potentially forward the request to the leader
        leaderAddr := k.raft.Leader()
        if leaderAddr == "" {
             return nil, errors.New("cannot create job: no leader available")
        }
         // Basic forwarding example (needs implementation):
         // return forwardRequestToLeader(leaderAddr, "/api/v1/print_jobs", reqJob)
        return nil, fmt.Errorf("cannot create job: this node (%s) is not the leader (%s)", k.nodeID, leaderAddr)
    }

    // --- Validation ---
    k.fsm.mu.RLock() // Read Lock for validation checks
    defer k.fsm.mu.RUnlock()

    // 1. Check if Printer exists
    if _, exists := k.fsm.printers[reqJob.PrinterID]; !exists {
        return nil, fmt.Errorf("validation failed: printer with ID '%s' not found", reqJob.PrinterID)
    }

    // 2. Check if Filament exists
    filament, exists := k.fsm.filaments[reqJob.FilamentID]
    if !exists {
        return nil, fmt.Errorf("validation failed: filament with ID '%s' not found", reqJob.FilamentID)
    }

    // 3. Check filament weight
    // Calculate weight used by other active (Queued/Running) jobs for the same filament
    committedWeight := 0
    for _, job := range k.fsm.printJobs {
        if job.FilamentID == reqJob.FilamentID && (job.Status == StatusQueued || job.Status == StatusRunning) {
            committedWeight += job.PrintWeightInGrams
        }
    }

    availableWeight := filament.RemainingWeightInGrams - committedWeight
    if reqJob.PrintWeightInGrams <= 0 {
         return nil, fmt.Errorf("validation failed: print_weight_in_grams must be positive")
    }
    if reqJob.PrintWeightInGrams > availableWeight {
        return nil, fmt.Errorf("validation failed: requested weight %dg exceeds available filament weight %dg (Total: %dg, Committed: %dg)",
            reqJob.PrintWeightInGrams, availableWeight, filament.RemainingWeightInGrams, committedWeight)
    }
    // --- End Validation ---


    // --- Create the Job ---
    // Generate a unique ID for the job
    jobID := uuid.New().String() // Use UUID for unique IDs
    newJob := PrintJob{
        ID:                 jobID,
        PrinterID:          reqJob.PrinterID,
        FilamentID:         reqJob.FilamentID,
        PrintWeightInGrams: reqJob.PrintWeightInGrams,
        Status:             StatusQueued, // Initialize status to Queued
        CreatedAt:          time.Now().UTC(),
    }


    // Create Raft command
    cmd := Command{
        Op:    "add_print_job",
        Key:   newJob.ID, // Use job ID as key for simplicity, or keep empty
        Value: string(mustMarshal(newJob)), // Marshal the full job object
    }

    data, err := json.Marshal(cmd)
    if err != nil {
        log.Printf("Error marshalling add_print_job command: %v", err)
        return nil, fmt.Errorf("internal server error: failed to create command")
    }

    // Apply command via Raft
    applyFuture := k.raft.Apply(data, 10*time.Second)
    if err := applyFuture.Error(); err != nil {
         log.Printf("Error applying add_print_job command via Raft: %v", err)
        return nil, fmt.Errorf("failed to commit print job via Raft: %w", err)
    }

    // Check Apply response for potential errors from the FSM Apply method itself
    resp := applyFuture.Response()
    if applyErr, ok := resp.(error); ok && applyErr != nil {
        log.Printf("Error returned from FSM Apply for add_print_job: %v", applyErr)
        return nil, fmt.Errorf("failed to apply print job state change: %w", applyErr)
    }


    log.Printf("Successfully submitted add_print_job command for ID: %s", newJob.ID)
    // Return the created job structure (reflecting the state *before* FSM applies it)
    // The FSM will update its state asynchronously.
    return &newJob, nil
}

func (s *HTTPServer) handleFilaments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "POST":
		var filament Filament
		if err := json.NewDecoder(r.Body).Decode(&filament); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := s.store.AddFilament(filament); err != nil {
			if strings.Contains(err.Error(), "not leader") {
				http.Error(w, err.Error(), http.StatusTemporaryRedirect)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusCreated)

	case "GET":
		// Directly read from the FSM's local state
		filaments, err := s.store.GetFilaments()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		json.NewEncoder(w).Encode(filaments)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}


func (s *HTTPServer) handlePrintJobs(w http.ResponseWriter, r *http.Request) {
    switch r.Method {
    case "POST":
        s.handleCreatePrintJob(w, r)
    // Add GET later if needed:
    // case "GET":
    //  s.handleListPrintJobs(w, r)
    default:
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
    }
}

func (s *HTTPServer) handleCreatePrintJob(w http.ResponseWriter, r *http.Request) {
    var reqJob PrintJob // Use PrintJob struct directly for input

    // Decode request body
    if err := json.NewDecoder(r.Body).Decode(&reqJob); err != nil {
        http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
        return
    }

     // Basic input sanity checks (beyond FSM validation)
    if reqJob.PrinterID == "" || reqJob.FilamentID == "" || reqJob.PrintWeightInGrams <= 0 {
         http.Error(w, "Missing or invalid required fields: printer_id, filament_id, print_weight_in_grams (must be > 0)", http.StatusBadRequest)
        return
    }
    // Ignore any status set by the user
    reqJob.Status = "" // Clear any status potentially sent by client


    // Call the KVStore method to add the job
    createdJob, err := s.store.AddPrintJob(reqJob)
    if err != nil {
        // Check for specific error types (e.g., validation)
        if strings.Contains(err.Error(), "validation failed:") {
            http.Error(w, err.Error(), http.StatusConflict) // 409 Conflict for validation errors
        } else if strings.Contains(err.Error(), "not the leader") {
             // Handle redirection or leader info hint
             leader := s.store.GetLeader()
             if leader != "" {
                 // Suggest redirecting (client needs to handle this)
                 w.Header().Set("Location", fmt.Sprintf("http://%s%s", leader, r.URL.Path)) // Assuming leader exposes HTTP on same path
                 http.Error(w, fmt.Sprintf("Not leader. Current leader is %s. Redirect suggested.", leader), http.StatusTemporaryRedirect) // 307
             } else {
                 http.Error(w, "Not leader, and leader unknown.", http.StatusServiceUnavailable) // 503
             }
        } else {
            // General internal errors
             log.Printf("Error creating print job: %v", err)
            http.Error(w, fmt.Sprintf("Failed to create print job: %v", err), http.StatusInternalServerError)
        }
        return
    }

    // Success
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusCreated) // 201 Created
    json.NewEncoder(w).Encode(createdJob) // Return the details of the created job
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
