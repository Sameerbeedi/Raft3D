# Raft3D

## Steps to Run

### Imports
```Go
import (
	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb/v2"
)
```

### Setup and Execution
```bash
# Initialize a new Go module with the name "raft.go"
# This creates a go.mod file to manage dependencies
go mod init main.go

# Download and install all dependencies required by the project
# The -a flag forces downloading of dependencies even if they're already cached
go get -a

# Compile and execute the raft.go file
# This runs the Raft consensus implementation
go run main.go
```
To test create printer, create filament, list all printers and list all filaments:

NOTE 1: All below commands to be used in powershell.

Start your raft cluster with all the nodes-
go run .\main.go node1 127.0.0.1:7000 :8000 (Bootstrapping the cluster)
go run .\main.go node2 127.0.0.1:7001 :8001 127.0.0.1:8000 (adding nodes)
etc.

NOTE 2: RAFT saves its state so if rerunning the cluster, remove the join_addr field while starting the nodes.
go run .\main.go node1 127.0.0.1:7000 :8000 (Bootstrapping the cluster)
go run .\main.go node2 127.0.0.1:7001 :8001 (adding nodes but join_addr field is removed)
etc.

CREATE PRINTER: (only works with leaders as it is a POST request and duplicates are handled)

$printer = @{
    id = "printer1"
    company = "Creality"
    model = "Ender 3"
} | ConvertTo-Json -Depth 10

Invoke-RestMethod -Uri http://127.0.0.1:8000/api/v1/printers -Method POST -Body $printer -ContentType "application/json"

LIST ALL PRINTERS: (works with any node as it is a GET request)

Invoke-RestMethod -Uri http://127.0.0.1:8000/api/v1/printers -Method GET

CREATE FILAMENT: (only works with leaders as it is a POST request and duplicates are handled)

$filament = @{
    id = "filament1"
    type = "PLA"
    color = "Blue"
    total_weight_in_grams = 1000
    remaining_weight_in_grams = 1000
} | ConvertTo-Json -Depth 10

Invoke-RestMethod -Uri http://127.0.0.1:8000/api/v1/filaments -Method POST -Body $filament -ContentType "application/json"

LIST ALL FILAMENTS: (works with any node as it is a GET request)

Invoke-RestMethod -Uri http://127.0.0.1:8000/api/v1/filaments -Method GET