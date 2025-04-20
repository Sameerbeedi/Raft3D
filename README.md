# Raft3D

A distributed key-value store and 3D print job manager built using the Raft consensus algorithm in Go.

## Overview

This application implements a fault-tolerant cluster using `hashicorp/raft`. It provides an HTTP API to manage:
* Basic key-value data
* 3D Printer inventory
* Filament inventory
* Print Job queueing and validation

Data is replicated across all nodes in the cluster for high availability. Write operations are handled by the leader node to ensure consistency.

## Setup

### Prerequisites

* Go (version 1.18 or later recommended)

### Dependencies

The project uses the following main Go modules:
* `github.com/hashicorp/raft`
* `github.com/hashicorp/raft-boltdb/v2`
* `github.com/google/uuid`

### Installation

1.  **Clone/Download:** Get the project files onto your local machine.
2.  **Navigate:** Open your terminal in the project's root directory (`Raft3D`).
3.  **Initialize Module (if needed):** If a `go.mod` file doesn't exist:
    ```bash
    go mod init Raft3D
    ```
4.  **Tidy Dependencies:** Download and install the required dependencies:
    ```bash
    go mod tidy
    ```
    Alternatively, use `go get -a`.

## Running the Cluster

Each node in the cluster needs to be run as a separate process.

### Starting the First Node (Bootstrapping)

The first node initializes the cluster. Run it without specifying a join address.

```bash
# Format: go run ./main.go <node-id> <raft-addr>:<raft-port> :<http-port>
# Example:
go run ./main.go node1 127.0.0.1:7000 :8000
```

### Starting Subsequent Nodes (Joining)

Start other nodes and point them to the HTTP address of an *existing* node (preferably the leader) in the cluster using the final argument.

```bash
# Format: go run ./main.go <node-id> <raft-addr>:<raft-port> :<http-port> <join-http-addr>:<join-http-port>
# Example joining node1 (running on HTTP port 8000):
go run ./main.go node2 127.0.0.1:7001 :8001 127.0.0.1:8000
go run ./main.go node3 127.0.0.1:7002 :8002 127.0.0.1:8000
```

**Important Note on Restarting:** Raft saves its state in `./node_<node-id>` directories. If you stop nodes and restart them later, they will attempt to reconnect based on their saved state. When restarting an existing cluster node, *omit* the join address argument.

```bash
# Example restarting node2 (assuming node1 is also restarting/running):
go run ./main.go node2 127.0.0.1:7001 :8001
```

## API Endpoints

Interact with the cluster via the HTTP port specified for each node. Write operations (POST, PUT, DELETE) must generally be sent to the **leader** node. Read operations (GET) can usually be sent to any node.

### Cluster

* **`GET /status`**
    * Description: Get the Raft status of the queried node (leader address, leader status, node ID).
    * Example (`curl`):
        ```bash
        curl http://127.0.0.1:8000/status
        ```

* **`POST /join`**
    * Description: Request the receiving node (must be leader) to add a new node to the cluster.
    * Body (JSON): `{"node_id": "nodeX", "addr": "raft_ip:raft_port"}`
    * Example (`curl`):
        ```bash
        curl -X POST -H "Content-Type: application/json"              -d '{"node_id": "node4", "addr": "127.0.0.1:7003"}'              http://127.0.0.1:8000/join
        ```

### Printers

* **`GET /api/v1/printers`**
    * Description: List all registered printers.
    * Example (`curl`):
        ```bash
        curl http://127.0.0.1:8000/api/v1/printers
        ```

* **`POST /api/v1/printers`**
    * Description: Add a new printer (Leader Only). Duplicates (by ID) are rejected.
    * Body (JSON): `{"id": "unique_printer_id", "company": "...", "model": "..."}`
    * Example (`curl`):
        ```bash
        curl -X POST -H "Content-Type: application/json"              -d '{"id": "printer1", "company": "Creality", "model": "Ender 3"}'              http://127.0.0.1:8000/api/v1/printers
        ```
    * PowerShell Example:
        ```powershell
        $printer = @{
            id = "printer1"
            company = "Creality"
            model = "Ender 3"
        } | ConvertTo-Json -Depth 10

        Invoke-RestMethod -Uri http://127.0.0.1:8000/api/v1/printers -Method POST -Body $printer -ContentType "application/json"
        ```

### Filaments

* **`GET /api/v1/filaments`**
    * Description: List all registered filaments.
    * Example (`curl`):
        ```bash
        curl http://127.0.0.1:8000/api/v1/filaments
        ```

* **`POST /api/v1/filaments`**
    * Description: Add a new filament spool (Leader Only). Duplicates (by ID) are rejected.
    * Body (JSON): `{"id": "unique_filament_id", "type": "PLA/ABS/PETG...", "color": "...", "total_weight_in_grams": 1000, "remaining_weight_in_grams": 1000}`
    * Example (`curl`):
        ```bash
        curl -X POST -H "Content-Type: application/json"              -d '{"id": "filament1", "type": "PLA", "color": "Blue", "total_weight_in_grams": 1000, "remaining_weight_in_grams": 1000}'              http://127.0.0.1:8000/api/v1/filaments
        ```
    * PowerShell Example:
        ```powershell
        $filament = @{
            id = "filament1"
            type = "PLA"
            color = "Blue"
            total_weight_in_grams = 1000
            remaining_weight_in_grams = 1000
        } | ConvertTo-Json -Depth 10

        Invoke-RestMethod -Uri http://127.0.0.1:8000/api/v1/filaments -Method POST -Body $filament -ContentType "application/json"
        ```

### Print Jobs

* **`POST /api/v1/print_jobs`**
    * Description: Create/queue a new print job (Leader Only).
    * Validation:
        * Checks if `printer_id` and `filament_id` exist.
        * Checks if `print_weight_in_grams` is positive.
        * Checks if the filament has enough `remaining_weight_in_grams` (considering weight committed by other `Queued` or `Running` jobs using the same filament).
    * Behavior: Initializes job status to `Queued`. User cannot set status during creation.
    * Body (JSON): `{"printer_id": "existing_printer_id", "filament_id": "existing_filament_id", "print_weight_in_grams": 50}`
    * Example (`curl`):
        ```bash
        curl -X POST -H "Content-Type: application/json"              -d '{"printer_id": "printer1", "filament_id": "filament1", "print_weight_in_grams": 150}'              http://127.0.0.1:8000/api/v1/print_jobs
        ```

### Basic Key-Value Store

* **`GET /kv/{key}`**
    * Description: Get the value for `{key}`.
    * Example (`curl`): `curl http://127.0.0.1:8000/kv/mykey`

* **`PUT /kv/{key}`**
    * Description: Set the value for `{key}` (Leader Only). Value is the raw request body.
    * Example (`curl`): `curl -X PUT -d 'myvalue' http://127.0.0.1:8000/kv/mykey`

* **`DELETE /kv/{key}`**
    * Description: Delete the key `{key}` (Leader Only).
    * Example (`curl`): `curl -X DELETE http://127.0.0.1:8000/kv/mykey`
