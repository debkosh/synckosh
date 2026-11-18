// Command device is a Go synckosh device for the js tests, to check the two implementations
// against each other.
//
//	device between              reads [[a, b], ...] and writes [key or "!error", ...]
//	device sync URL TOKEN PASS  joins the bucket with the passphrase, puts the items it reads
//	                            ([{collection, datatype, public, private}]), syncs, and writes
//	                            the live items of their collection
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/bolt"
	"github.com/sudiptadeb/synckosh/client"
)

func main() {
	log.SetFlags(0)
	enc := json.NewEncoder(os.Stdout)
	switch os.Args[1] {
	case "between":
		var pairs [][2]string
		if err := json.NewDecoder(os.Stdin).Decode(&pairs); err != nil {
			log.Fatal(err)
		}
		out := make([]string, len(pairs))
		for i, p := range pairs {
			k, err := synckosh.Between(p[0], p[1])
			switch {
			case errors.Is(err, synckosh.ErrNoRoom):
				k = "!noroom"
			case err != nil:
				k = "!error"
			}
			out[i] = k
		}
		enc.Encode(out)
	case "sync":
		ctx := context.Background()
		c := &client.Client{URL: os.Args[2], Token: os.Args[3]}
		key, err := client.Join(ctx, c, os.Args[4])
		if err != nil {
			log.Fatal(err)
		}
		dir, err := os.MkdirTemp("", "synckosh-js-device")
		if err != nil {
			log.Fatal(err)
		}
		defer os.RemoveAll(dir)
		local, err := bolt.OpenLocal(filepath.Join(dir, "sync.db"))
		if err != nil {
			log.Fatal(err)
		}
		r, err := client.Open(client.Config{Client: c, Local: local, Key: key})
		if err != nil {
			log.Fatal(err)
		}
		var puts []synckosh.Item
		if err := json.NewDecoder(os.Stdin).Decode(&puts); err != nil {
			log.Fatal(err)
		}
		if err := r.Sync(ctx); err != nil {
			log.Fatal(err)
		}
		if _, err := r.Put(puts...); err != nil {
			log.Fatal(err)
		}
		if err := r.Sync(ctx); err != nil {
			log.Fatal(err)
		}
		items, err := r.Items(puts[0].Collection)
		if err != nil {
			log.Fatal(err)
		}
		enc.Encode(items)
	default:
		log.Fatalf("device: %s?", os.Args[1])
	}
}
