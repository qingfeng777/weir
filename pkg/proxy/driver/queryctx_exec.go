package driver

import (
	"context"
	"strings"

	"github.com/pingcap-incubator/weir/pkg/proxy/server"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/logutil"
	gomysql "github.com/siddontang/go-mysql/mysql"
	"go.uber.org/zap"
)

func (q *QueryCtxImpl) execute(ctx context.Context, sql string) ([]server.ResultSet, error) {
	charsetInfo, collation := q.sessionVars.GetCharsetInfo()
	stmt, err := q.parser.ParseOneStmt(sql, charsetInfo, collation)
	if err != nil {
		return nil, err
	}

	return q.executeStmt(ctx, sql, stmt)
}

// TODO: implement this function
func (q *QueryCtxImpl) executeStmt(ctx context.Context, sql string, stmtNode ast.StmtNode) ([]server.ResultSet, error) {
	switch stmt := stmtNode.(type) {
	case *ast.SetStmt:
		return nil, q.setVariable(ctx, stmt)
	case *ast.UseStmt:
		err := q.useDB(ctx, stmt.DBName)
		return nil, err
	case *ast.ShowStmt:
		return q.executeShowStmt(ctx, sql, stmt)
	case *ast.SelectStmt:
		return q.executeInBackend(ctx, sql, stmtNode)
	case *ast.BeginStmt:
		return nil, q.begin(ctx)
	case *ast.CommitStmt:
		return nil, q.commitOrRollback(ctx, true)
	case *ast.RollbackStmt:
		return nil, q.commitOrRollback(ctx, false)
	default:
		return nil, mysql.NewErrf(mysql.ErrUnknown, "stmt %T not supported now", stmtNode)
	}
}

// TODO: implement this function
func (q *QueryCtxImpl) executeShowStmt(ctx context.Context, sql string, stmt *ast.ShowStmt) ([]server.ResultSet, error) {
	switch stmt.Tp {
	case ast.ShowDatabases:
		databases := q.ns.Frontend().ListDatabases()
		result, err := createShowDatabasesResult(databases)
		if err != nil {
			return nil, err
		}
		return []server.ResultSet{wrapMySQLResult(result)}, nil
	case ast.ShowTables:
		return q.executeInBackend(ctx, sql, stmt)
	default:
		return nil, mysql.NewErrf(mysql.ErrUnknown, "show type %v not supported now", stmt.Tp)
	}
}

func createShowDatabasesResult(dbNames []string) (*gomysql.Result, error) {
	var values [][]interface{}
	for _, db := range dbNames {
		values = append(values, []interface{}{db})
	}

	rs, err := gomysql.BuildSimpleTextResultset([]string{"Database"}, values)
	if err != nil {
		return nil, err
	}

	result := &gomysql.Result{
		Status:    0,
		Resultset: rs,
	}

	// copied from go-mysql client/conn.readResultRows()
	// since convertFieldsToColumnInfos() only read result.Value,
	// so we have to write row data back to value
	// FIXME(eastfisher): remove this when weir client is finished
	if cap(result.Values) < len(result.RowDatas) {
		result.Values = make([][]gomysql.FieldValue, len(result.RowDatas))
	} else {
		result.Values = result.Values[:len(result.RowDatas)]
	}

	for i := range result.Values {
		result.Values[i], err = result.RowDatas[i].Parse(result.Fields, false, result.Values[i])

		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	return result, nil
}

func (q *QueryCtxImpl) executeInBackend(ctx context.Context, sql string, stmtNode ast.StmtNode) ([]server.ResultSet, error) {
	conn, err := q.getBackendConn(ctx)
	if err != nil {
		return nil, err
	}
	defer q.releaseBackendConn(conn)

	if err := conn.UseDB(q.currentDB); err != nil {
		return nil, err
	}

	result, err := conn.Execute(sql)
	if err != nil {
		return nil, err
	}

	resultSet := wrapMySQLResult(result)
	return []server.ResultSet{resultSet}, nil
}

func (q *QueryCtxImpl) getBackendConn(ctx context.Context) (PooledBackendConn, error) {
	conn, err := q.ns.Backend().GetPooledConn(ctx)
	return conn, err
}

func (q *QueryCtxImpl) releaseBackendConn(conn PooledBackendConn) {
	conn.PutBack()
}

func (q *QueryCtxImpl) useDB(ctx context.Context, db string) error {
	if !q.ns.Frontend().IsDatabaseAllowed(db) {
		return mysql.NewErrf(mysql.ErrDBaccessDenied, "db %s access denied", db)
	}
	q.currentDB = db
	return nil
}

func (q *QueryCtxImpl) setVariable(ctx context.Context, stmt *ast.SetStmt) error {
	for _, v := range stmt.Variables {
		switch strings.ToLower(v.Name) {
		case variable.AutoCommit:
			return q.setAutoCommit(ctx, v)
		}
	}
	return nil
}

func (q *QueryCtxImpl) setAutoCommit(ctx context.Context, v *ast.VariableAssignment) error {
	q.txnLock.Lock()
	defer q.txnLock.Unlock()

	var err error
	autocommit, err := getAutoCommitValue(v.Value)
	if err != nil {
		return err
	}

	originAutoCommit := q.isAutoCommit()
	defer func() {
		if err != nil {
			q.sessionVars.SetStatusFlag(mysql.ServerStatusAutocommit, originAutoCommit)
		}
		q.postUseTxnConn(err)
	}()

	q.sessionVars.SetStatusFlag(mysql.ServerStatusAutocommit, autocommit)
	return q.initTxnConn(ctx)
}

func getAutoCommitValue(v ast.ExprNode) (bool, error) {
	if _, ok := v.(*ast.DefaultExpr); ok {
		return true, nil
	}
	value, ok := v.(ast.ValueExpr)
	if !ok {
		return false, errors.Errorf("invalid autocommit value type %T", v)
	}
	autocommitInt64, ok := value.GetValue().(int64)
	if !ok {
		return false, errors.Errorf("autocommit value is not int64, type %T", value.GetValue())
	}
	return autocommitInt64 == 1, nil
}

func (q *QueryCtxImpl) begin(ctx context.Context) error {
	q.txnLock.Lock()
	defer q.txnLock.Unlock()

	var err error
	defer func() {
		q.postUseTxnConn(err)
	}()

	if err = q.initTxnConn(ctx); err != nil {
		return err
	}

	if err = q.txnConn.Begin(); err != nil {
		return err
	}

	q.sessionVars.SetStatusFlag(mysql.ServerStatusInTrans, true)
	return nil
}

func (q *QueryCtxImpl) commitOrRollback(ctx context.Context, commit bool) error {
	q.txnLock.Lock()
	defer q.txnLock.Unlock()

	if q.txnConn == nil {
		q.sessionVars.SetStatusFlag(ServerStatusInTrans, false)
		return errors.New("txn conn is not set")
	}

	var err error
	defer func() {
		q.postUseTxnConn(err)
	}()

	if commit {
		err = q.txnConn.Commit()
	} else {
		err = q.txnConn.Rollback()
	}
	if err != nil {
		return err
	}

	q.sessionVars.SetStatusFlag(ServerStatusInTrans, false)
	return nil
}

func (q *QueryCtxImpl) initTxnConn(ctx context.Context) error {
	if q.txnConn != nil {
		return nil
	}
	conn, err := q.ns.Backend().GetPooledConn(ctx)
	if err != nil {
		return err
	}
	if err := conn.SetAutoCommit(q.isAutoCommit()); err != nil {
		return err
	}
	q.txnConn = conn
	return nil
}

func (q *QueryCtxImpl) postUseTxnConn(err error) {
	if err != nil {
		if q.txnConn != nil {
			if errClose := q.txnConn.Close(); errClose != nil {
				logutil.BgLogger().Error("close txn conn error", zap.Error(errClose), zap.String("namespace", q.ns.Name()))
			}
			q.txnConn = nil
		}
		q.sessionVars.SetStatusFlag(ServerStatusInTrans, false)
	} else {
		if q.isAutoCommit() && !q.isInTransaction() && q.txnConn != nil {
			q.txnConn.PutBack()
			q.txnConn = nil
		}
	}
}

func (q *QueryCtxImpl) isAutoCommit() bool {
	return q.sessionVars.GetStatusFlag(ServerStatusAutocommit)
}

func (q *QueryCtxImpl) isInTransaction() bool {
	return q.sessionVars.GetStatusFlag(ServerStatusInTrans)
}
