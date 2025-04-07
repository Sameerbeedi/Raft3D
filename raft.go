package main

import (
	"fmt"

	"github.com/hashicorp/raft"
	// "github.com/hashicorp/raft-boltdb"
)

func main() {
	raftConfig := raft.DefaultConfig()
	raftConfig.LocalID = raft.ServerID("node1")
	fmt.Print("raftConfig: ", raftConfig)
}
