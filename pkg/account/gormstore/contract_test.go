package gormstore_test

import (
	"github.com/ydtg1993/papa/v4/pkg/account/contract"
	"github.com/ydtg1993/papa/v4/pkg/account/gormstore"
)

var _ contract.PoolStore = (*gormstore.GORMPoolStore)(nil)
