package bolt

import (
	"path/filepath"
	"testing"

	"github.com/sudiptadeb/synckosh/server"
	"github.com/sudiptadeb/synckosh/server/storetest"
)

func TestStore(t *testing.T) {
	storetest.Run(t, func(t *testing.T) server.Store {
		st, err := OpenStore(filepath.Join(t.TempDir(), "server.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	})
}
