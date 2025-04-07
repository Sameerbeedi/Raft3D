# Raft3D

## Steps to Run

### Imports
```Go
import (
	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
)
```

### Setup and Execution
```bash
# Initialize a new Go module with the name "raft.go"
# This creates a go.mod file to manage dependencies
go mod init raft.go

# Download and install all dependencies required by the project
# The -a flag forces downloading of dependencies even if they're already cached
go get -a

# Compile and execute the raft.go file
# This runs the Raft consensus implementation
go run raft.go
```