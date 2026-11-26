package bolt

import (
	"path/filepath"
	"testing"

	"github.com/debkosh/synckosh/server"
	"github.com/debkosh/synckosh/server/storetest"
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
