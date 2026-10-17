package memory

import (
	"testing"

	"github.com/frappe/pathway/internal/repository"
	"github.com/frappe/pathway/internal/repository/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) repository.Store { return New().Repositories() })
}
