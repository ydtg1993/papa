package gormstore_test

import (
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/account/contract"
	"github.com/ydtg1993/papa/v4/pkg/account/gormstore"
	"testing"
)

// Assignments exercise type identity, not merely equal struct layouts.
var (
	_ account.PoolStore        = (*gormstore.GORMPoolStore)(nil)
	_ contract.PoolStore       = (*account.GORMPoolStore)(nil)
	_ account.PoolState        = contract.PoolState{}
	_ account.ManagementState  = contract.ManagementState{}
	_ account.PoolRecord       = gormstore.PoolRecord{}
	_ account.ManagementRecord = gormstore.ManagementRecord{}
)

func TestCompatibilityFacadePreservesModelsAndConflictIdentity(t *testing.T) {
	if account.ErrPoolConflict != contract.ErrPoolConflict {
		t.Fatal("CAS error identity changed")
	}
	if len(account.Models()) != len(gormstore.Models()) {
		t.Fatal("migration models changed")
	}
	if account.NewGORMPoolStore(nil) == nil {
		t.Fatal("compatibility constructor returned nil")
	}
}
