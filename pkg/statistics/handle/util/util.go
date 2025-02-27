// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"context"
	"strconv"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta/model"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser/terror"
	"github.com/pingcap/tidb/pkg/planner/core/resolve"
	"github.com/pingcap/tidb/pkg/sessionctx"
	"github.com/pingcap/tidb/pkg/sessionctx/vardef"
	"github.com/pingcap/tidb/pkg/sessionctx/variable"
	"github.com/pingcap/tidb/pkg/types"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/chunk"
	"github.com/pingcap/tidb/pkg/util/intest"
	"github.com/pingcap/tidb/pkg/util/sqlexec"
	"github.com/pingcap/tidb/pkg/util/sqlexec/mock"
	"github.com/tikv/client-go/v2/oracle"
)

const (
	// StatsMetaHistorySourceAnalyze indicates stats history meta source from analyze
	StatsMetaHistorySourceAnalyze = "analyze"
	// StatsMetaHistorySourceLoadStats indicates stats history meta source from load stats
	StatsMetaHistorySourceLoadStats = "load stats"
	// StatsMetaHistorySourceFlushStats indicates stats history meta source from flush stats
	StatsMetaHistorySourceFlushStats = "flush stats"
	// StatsMetaHistorySourceSchemaChange indicates stats history meta source from schema change
	StatsMetaHistorySourceSchemaChange = "schema change"
	// StatsMetaHistorySourceExtendedStats indicates stats history meta source from extended stats
	StatsMetaHistorySourceExtendedStats = "extended stats"
)

var (
	// UseCurrentSessionOpt to make sure the sql is executed in current session.
	UseCurrentSessionOpt = []sqlexec.OptionFuncAlias{sqlexec.ExecOptionUseCurSession}

	// StatsCtx is used to mark the request is from stats module.
	StatsCtx = kv.WithInternalSourceType(context.Background(), kv.InternalTxnStats)
)

// finishTransaction will execute `commit` when error is nil, otherwise `rollback`.
func finishTransaction(sctx sessionctx.Context, err error) error {
	if err == nil {
		_, _, err = ExecRows(sctx, "COMMIT")
	} else {
		_, _, err1 := ExecRows(sctx, "rollback")
		terror.Log(errors.Trace(err1))
	}
	return errors.Trace(err)
}

var (
	// FlagWrapTxn indicates whether to wrap a transaction.
	FlagWrapTxn = 0
)

// CallWithSCtx allocates a sctx from the pool and call the f().
func CallWithSCtx(pool util.DestroyableSessionPool, f func(sctx sessionctx.Context) error, flags ...int) (err error) {
	defer util.Recover(metrics.LabelStats, "CallWithSCtx", nil, false)
	se, err := pool.Get()
	if err != nil {
		return errors.Trace(err)
	}
	defer func() {
		if err == nil { // only recycle when no error
			pool.Put(se)
		} else {
			// Note: Otherwise, the session will be leaked.
			pool.Destroy(se)
		}
	}()
	sctx := se.(sessionctx.Context)
	if err := UpdateSCtxVarsForStats(sctx); err != nil { // update stats variables automatically
		return errors.Trace(err)
	}

	wrapTxn := false
	for _, flag := range flags {
		if flag == FlagWrapTxn {
			wrapTxn = true
		}
	}
	if wrapTxn {
		err = WrapTxn(sctx, f)
	} else {
		err = f(sctx)
	}
	return errors.Trace(err)
}

// UpdateSCtxVarsForStats updates all necessary variables that may affect the behavior of statistics.
func UpdateSCtxVarsForStats(sctx sessionctx.Context) error {
	// async merge global stats
	enableAsyncMergeGlobalStats, err := sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBEnableAsyncMergeGlobalStats)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().EnableAsyncMergeGlobalStats = variable.TiDBOptOn(enableAsyncMergeGlobalStats)

	// concurrency of save stats to storage
	analyzePartitionConcurrency, err := sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBAnalyzePartitionConcurrency)
	if err != nil {
		return err
	}
	c, err := strconv.ParseInt(analyzePartitionConcurrency, 10, 64)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().AnalyzePartitionConcurrency = int(c)

	// analyzer version
	verInString, err := sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBAnalyzeVersion)
	if err != nil {
		return err
	}
	ver, err := strconv.ParseInt(verInString, 10, 64)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().AnalyzeVersion = int(ver)

	// enable historical stats
	val, err := sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBEnableHistoricalStats)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().EnableHistoricalStats = variable.TiDBOptOn(val)

	// partition mode
	pruneMode, err := sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBPartitionPruneMode)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().PartitionPruneMode.Store(pruneMode)

	// enable analyze snapshot
	analyzeSnapshot, err := sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBEnableAnalyzeSnapshot)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().EnableAnalyzeSnapshot = variable.TiDBOptOn(analyzeSnapshot)

	// enable skip column types
	val, err = sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBAnalyzeSkipColumnTypes)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().AnalyzeSkipColumnTypes = variable.ParseAnalyzeSkipColumnTypes(val)

	// skip missing partition stats
	val, err = sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBSkipMissingPartitionStats)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().SkipMissingPartitionStats = variable.TiDBOptOn(val)
	verInString, err = sctx.GetSessionVars().GlobalVarsAccessor.GetGlobalSysVar(vardef.TiDBMergePartitionStatsConcurrency)
	if err != nil {
		return err
	}
	ver, err = strconv.ParseInt(verInString, 10, 64)
	if err != nil {
		return err
	}
	sctx.GetSessionVars().AnalyzePartitionMergeConcurrency = int(ver)
	return nil
}

// GetCurrentPruneMode returns the current latest partitioning table prune mode.
func GetCurrentPruneMode(pool util.DestroyableSessionPool) (mode string, err error) {
	err = CallWithSCtx(pool, func(sctx sessionctx.Context) error {
		mode = sctx.GetSessionVars().PartitionPruneMode.Load()
		return nil
	})
	return
}

// WrapTxn uses a transaction here can let different SQLs in this operation have the same data visibility.
func WrapTxn(sctx sessionctx.Context, f func(sctx sessionctx.Context) error) (err error) {
	// TODO: check whether this sctx is already in a txn
	if _, _, err := ExecRows(sctx, "BEGIN PESSIMISTIC"); err != nil {
		return err
	}
	defer func() {
		err = finishTransaction(sctx, err)
	}()
	err = f(sctx)
	return
}

// GetStartTS gets the start ts from current transaction.
func GetStartTS(sctx sessionctx.Context) (uint64, error) {
	txn, err := sctx.Txn(true)
	if err != nil {
		return 0, err
	}
	return txn.StartTS(), nil
}

// Exec is a helper function to execute sql and return RecordSet.
func Exec(sctx sessionctx.Context, sql string, args ...any) (sqlexec.RecordSet, error) {
	return ExecWithCtx(StatsCtx, sctx, sql, args...)
}

// ExecWithCtx is a helper function to execute sql and return RecordSet.
func ExecWithCtx(
	ctx context.Context,
	sctx sessionctx.Context,
	sql string,
	args ...any,
) (sqlexec.RecordSet, error) {
	sqlExec := sctx.GetSQLExecutor()
	// TODO: use RestrictedSQLExecutor + ExecOptionUseCurSession instead of SQLExecutor
	return sqlExec.ExecuteInternal(ctx, sql, args...)
}

// ExecRows is a helper function to execute sql and return rows and fields.
func ExecRows(sctx sessionctx.Context, sql string, args ...any) (rows []chunk.Row, fields []*resolve.ResultField, err error) {
	failpoint.Inject("ExecRowsTimeout", func() {
		failpoint.Return(nil, nil, errors.New("inject timeout error"))
	})
	return ExecRowsWithCtx(StatsCtx, sctx, sql, args...)
}

// ExecRowsWithCtx is a helper function to execute sql and return rows and fields.
func ExecRowsWithCtx(
	ctx context.Context,
	sctx sessionctx.Context,
	sql string,
	args ...any,
) (rows []chunk.Row, fields []*resolve.ResultField, err error) {
	if intest.InTest {
		if v := sctx.Value(mock.RestrictedSQLExecutorKey{}); v != nil {
			return v.(*mock.MockRestrictedSQLExecutor).ExecRestrictedSQL(
				StatsCtx, UseCurrentSessionOpt, sql, args...,
			)
		}
	}

	sqlExec := sctx.GetRestrictedSQLExecutor()
	return sqlExec.ExecRestrictedSQL(ctx, UseCurrentSessionOpt, sql, args...)
}

// ExecWithOpts is a helper function to execute sql and return rows and fields.
func ExecWithOpts(sctx sessionctx.Context, opts []sqlexec.OptionFuncAlias, sql string, args ...any) (rows []chunk.Row, fields []*resolve.ResultField, err error) {
	sqlExec := sctx.GetRestrictedSQLExecutor()
	return sqlExec.ExecRestrictedSQL(StatsCtx, opts, sql, args...)
}

// DurationToTS converts duration to timestamp.
func DurationToTS(d time.Duration) uint64 {
	return oracle.ComposeTS(d.Nanoseconds()/int64(time.Millisecond), 0)
}

// IsSpecialGlobalIndex checks a index is a special global index or not.
// A special global index is one that is a global index and has virtual generated columns or prefix columns.
func IsSpecialGlobalIndex(idx *model.IndexInfo, tblInfo *model.TableInfo) bool {
	if !idx.Global {
		return false
	}
	for _, col := range idx.Columns {
		colInfo := tblInfo.Columns[col.Offset]
		isPrefixCol := col.Length != types.UnspecifiedLength
		if colInfo.IsVirtualGenerated() || isPrefixCol {
			return true
		}
	}
	return false
}
