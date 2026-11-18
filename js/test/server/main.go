// Command server is a synckosh server for the js tests: a bolt store in a temporary folder, one
// bucket per bearer token, on a free local port that it prints as its first line. POST
// /test/reset?bucket= changes a bucket's store id, as after a loss of its data.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/sudiptadeb/synckosh/bolt"
	"github.com/sudiptadeb/synckosh/server"
)

func main() {
	dir, err := os.MkdirTemp("", "synckosh-js-test")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store, err := bolt.OpenStore(filepath.Join(dir, "server.db"))
	if err != nil {
		log.Fatal(err)
	}
	srv := server.New(store, server.Bearer(func(token string) (string, error) { return token, nil }))
	mux := http.NewServeMux()
	mux.Handle("/v1", srv)
	mux.Handle("/v1/", srv)
	mux.HandleFunc("POST /test/reset", func(w http.ResponseWriter, r *http.Request) {
		if err := srv.ResetStore(r.URL.Query().Get("bucket")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("http://%s\n", ln.Addr())
	log.Fatal(http.Serve(ln, mux))
}
