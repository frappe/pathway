//go:build live

package redis

import (
	"testing"

	"github.com/frappe/pathway/internal/repository"
	"github.com/frappe/pathway/internal/repository/storetest"
)

// The same contract memory answers to on every `go test`, against a real server.
func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) repository.Store {
		client, _ := liveStore(t)
		return client.Store()
	})
}
