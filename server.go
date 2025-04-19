// package main

// import (
// 	"fmt"
// 	"log"
// 	"net/http"
// )

// func Home(w http.ResponseWriter, r *http.Request) {
// 	fmt.Fprintf(w, "Welcome to the Raft Consensus Algorithm!")
// }

// func main() {
// 	http.HandleFunc("/", Home)
// 	fmt.Println("Server is running on port 8080...")
// 	if err := http.ListenAndServe(":8080", nil); err != nil {
// 		log.Fatal(err)
// 	}
// }
