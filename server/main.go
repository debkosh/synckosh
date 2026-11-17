// syncserver keeps each account's synced items: for each type and guid, the newest item. It
// never sees inside an item's data, which devices seal. The protocol is in
// SPEC.md.
//
//	syncserver -addr 127.0.0.1:8484 -db sync.db
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8484", "address to listen on; put TLS in front of it")
	dbPath := flag.String("db", "sync.db", "the SQLite file")
	flag.Parse()
	st, err := openStore(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	srv := &http.Server{
		Addr:              *addr,
		Handler:           newServer(st),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("syncserver: listening on %s, items in %s", *addr, *dbPath)
	log.Fatal(srv.ListenAndServe())
}
