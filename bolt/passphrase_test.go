package bolt

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sudiptadeb/synckosh"
	"github.com/sudiptadeb/synckosh/client"
)

func TestLookTellsWhatABucketHolds(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	at := func(bucket string) *client.Client {
		return &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{bucket}}}
	}
	look := func(bucket string, want client.BucketState) {
		t.Helper()
		if got, err := client.Look(ctx, at(bucket)); err != nil || got != want {
			t.Fatalf("%s: %v %v, want %v", bucket, got, err, want)
		}
	}
	look("me", client.BucketNew)
	if _, err := client.Create(ctx, at("me"), "correct horse battery staple", "Main"); err != nil {
		t.Fatal(err)
	}
	look("me", client.BucketKeyed)
	local, err := OpenLocal(e.dir + "/plain.db")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	plain, _ := client.Open(client.Config{Client: at("plain"), Local: local})
	b := bookmark("root", "Go")
	b.GUID = synckosh.NewGUID()
	if _, err := plain.Put(b); err != nil {
		t.Fatal(err)
	}
	if err := plain.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	look("plain", client.BucketPlain)
}

// A device that holds the key checks, changes and adds passphrases, and a new device joins with
// whichever is current.
func TestPassphrases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	key, err := client.Create(ctx, c, "the first passphrase", "Main")
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	mac := e.device("mac")
	sync(t, mac)
	if err := mac.CheckPassphrase("the first passphrase"); err != nil {
		t.Fatalf("the right passphrase: %v", err)
	}
	if err := mac.CheckPassphrase("wrong"); !errors.Is(err, synckosh.ErrPassphrase) {
		t.Fatalf("a wrong passphrase: %v", err)
	}

	code, made, err := mac.AddBackupCode("Printed")
	if err != nil || !made.BackupCode || made.Label != "Printed" || time.Since(made.Created) > time.Minute {
		t.Fatalf("%+v %v", made, err)
	}
	if err := mac.SetPassphrase("the second passphrase", "Main"); err != nil {
		t.Fatal(err)
	}
	sync(t, mac)
	ps, err := mac.Passphrases()
	if err != nil || len(ps) != 2 {
		t.Fatalf("passphrases: %+v %v, want the new one and the code", ps, err)
	}
	for _, p := range ps {
		if p.BackupCode != (p.Label == "Printed") {
			t.Errorf("%+v", p)
		}
	}
	if _, err := client.Join(ctx, c, "the first passphrase"); !errors.Is(err, synckosh.ErrPassphrase) {
		t.Fatalf("the old passphrase still joins: %v", err)
	}
	for _, p := range []string{"the second passphrase", code} {
		if k, err := client.Join(ctx, c, p); err != nil || k.ID != key.ID {
			t.Fatalf("join with %q: %v", p, err)
		}
	}
	if err := mac.CheckPassphrase("the second passphrase"); err != nil {
		t.Fatalf("the new passphrase: %v", err)
	}

	plain, _ := client.Open(client.Config{Client: c, Local: mac.local})
	if err := plain.CheckPassphrase("x"); !errors.Is(err, client.ErrNoKey) {
		t.Fatalf("without a key: %v", err)
	}
}

// A backup code is a second passphrase the app shows differently: a person who forgot the
// passphrase joins with the code, typed however, sets a new passphrase and keeps the code; and
// the app removes a code it replaces.
func TestBackupCodes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := &client.Client{URL: e.url, HTTP: &http.Client{Transport: signedIn{"me"}}}
	key, err := client.Create(ctx, c, "forgotten", "Main")
	if err != nil {
		t.Fatal(err)
	}
	e.key = key
	mac := e.device("mac")
	sync(t, mac)
	code, first, err := mac.AddBackupCode("Backup, 22 Nov")
	if err != nil {
		t.Fatal(err)
	}
	if norm, ok := synckosh.NormalizeCode(code); !ok || norm != code || len(code) != 29 {
		t.Fatalf("code %q", code)
	}
	if err := mac.CheckPassphrase(code); err != nil {
		t.Fatalf("check the code: %v", err)
	}
	sync(t, mac)

	// Lower case, no dashes, O for 0 and l for 1: the same code.
	typed := strings.NewReplacer("-", "", "0", "o", "1", "l").Replace(strings.ToLower(code))
	k, err := client.Join(ctx, c, typed)
	if err != nil || k.ID != key.ID {
		t.Fatalf("join with %q: %v", typed, err)
	}
	e.key = k
	phone := e.device("phone")
	sync(t, phone)
	if err := phone.SetPassphrase("remembered", "Main"); err != nil {
		t.Fatal(err)
	}
	sync(t, phone)
	for _, p := range []string{"remembered", code} {
		if _, err := client.Join(ctx, c, p); err != nil {
			t.Fatalf("join with %q: %v", p, err)
		}
	}

	// One code at a time is the app's choice: it adds the new one and removes the old.
	code2, second, err := phone.AddBackupCode("Backup, 23 Nov")
	if err != nil {
		t.Fatal(err)
	}
	if err := phone.RemovePassphrase(first.GUID); err != nil {
		t.Fatal(err)
	}
	if err := phone.RemovePassphrase(first.GUID); !errors.Is(err, client.ErrNoCopy) {
		t.Fatalf("removing it again: %v", err)
	}
	sync(t, phone)
	sync(t, mac)
	ps, err := mac.Passphrases()
	if err != nil || len(ps) != 2 {
		t.Fatalf("%+v %v, want the passphrase and the second code", ps, err)
	}
	for _, p := range ps {
		if p.BackupCode != (p.GUID == second.GUID) || p.Created.IsZero() {
			t.Errorf("%+v", p)
		}
	}
	if _, err := client.Join(ctx, c, code); !errors.Is(err, synckosh.ErrPassphrase) {
		t.Fatalf("the removed code still joins: %v", err)
	}
	if err := mac.CheckPassphrase(code2); err != nil {
		t.Fatalf("the second code: %v", err)
	}
	// A code only opens a code's copy in its written form; a passphrase's copy takes text as typed.
	if _, err := client.Join(ctx, c, strings.ToUpper("remembered")); !errors.Is(err, synckosh.ErrPassphrase) {
		t.Fatalf("a passphrase in another case joins: %v", err)
	}
}
