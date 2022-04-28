// SPDX-FileCopyrightText: 2021 The Go-SSB Authors
//
// SPDX-License-Identifier: MIT

//go:build nommio
// +build nommio

package repo

import (
	"github.com/dgraph-io/badger/v3"
)

func badgerOpts(dbPath string) badger.Options {
	return badger.DefaultOptions(dbPath).
		WithMemTableSize(1 << 20).
		WithValueLogFileSize(1 << 21).
		WithNumMemtables(1).
		WithNumLevelZeroTables(1).
		WithNumLevelZeroTablesStall(1).
		WithNumCompactors(2).
		WithIndexCacheSize(1 << 21).
		WithBlockCacheSize(1 << 21).
		WithLogger(nil)
}
