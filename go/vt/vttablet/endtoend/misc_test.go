/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endtoend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/mysql/replication"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/test/utils"
	"vitess.io/vitess/go/vt/callerid"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/log"
	querypb "vitess.io/vitess/go/vt/proto/query"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtenv"
	"vitess.io/vitess/go/vt/vttablet/endtoend/framework"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/schema"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/tabletenv"
)

func TestSimpleRead(t *testing.T) {
	vstart := framework.DebugVars()
	_, err := framework.NewClient().Execute("select * from vitess_test where intval=1", nil)
	require.NoError(t, err)
	vend := framework.DebugVars()
	compareIntDiff(t, vend, "Queries/TotalCount", vstart, 1)
	compareIntDiff(t, vend, "Queries/Histograms/Select/Count", vstart, 1)
}

func TestBinary(t *testing.T) {
	client := framework.NewClient()
	defer client.Execute("delete from vitess_test where intval in (4,5)", nil)

	binaryData := "\x00'\"\b\n\r\t\x1a\\\x00\x0f\xf0\xff"
	// Test without bindvars.
	_, err := client.Execute(
		"insert into vitess_test values "+
			"(4, null, null, '\\0\\'\\\"\\b\\n\\r\\t\\Z\\\\\x00\x0f\xf0\xff')",
		nil,
	)
	require.NoError(t, err)
	qr, err := client.Execute("select binval from vitess_test where intval=4", nil)
	require.NoError(t, err)
	want := sqltypes.Result{
		Fields: []*querypb.Field{
			{
				Name:         "binval",
				Type:         sqltypes.VarBinary,
				Table:        "vitess_test",
				OrgTable:     "vitess_test",
				Database:     "vttest",
				OrgName:      "binval",
				ColumnLength: 256,
				Charset:      63,
				Flags:        128,
			},
		},
		Rows: [][]sqltypes.Value{
			{
				sqltypes.NewVarBinary(binaryData),
			},
		},
		StatusFlags: sqltypes.ServerStatusAutocommit,
	}
	utils.MustMatch(t, want, *qr)

	// Test with bindvars.
	_, err = client.Execute(
		"insert into vitess_test values(5, null, null, :bindata)",
		map[string]*querypb.BindVariable{"bindata": sqltypes.StringBindVariable(binaryData)},
	)
	require.NoError(t, err)
	qr, err = client.Execute("select binval from vitess_test where intval=5", nil)
	require.NoError(t, err)
	assert.Truef(t, qr.Equal(&want), "Execute: \n%#v, want \n%#v", prettyPrint(*qr), prettyPrint(want))
}

func TestNocacheListArgs(t *testing.T) {
	client := framework.NewClient()
	query := "select * from vitess_test where intval in ::list"

	qr, err := client.Execute(
		query,
		map[string]*querypb.BindVariable{
			"list": sqltypes.TestBindVariable([]any{2, 3, 4}),
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, len(qr.Rows))

	qr, err = client.Execute(
		query,
		map[string]*querypb.BindVariable{
			"list": sqltypes.TestBindVariable([]any{3, 4}),
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, len(qr.Rows))

	qr, err = client.Execute(
		query,
		map[string]*querypb.BindVariable{
			"list": sqltypes.TestBindVariable([]any{3}),
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, len(qr.Rows))

	// Error case
	_, err = client.Execute(
		query,
		map[string]*querypb.BindVariable{
			"list": sqltypes.TestBindVariable([]any{}),
		},
	)
	assert.EqualError(t, err, "empty list supplied for list (CallerID: dev)")
}

func TestIntegrityError(t *testing.T) {
	vstart := framework.DebugVars()
	client := framework.NewClient()
	_, err := client.Execute("insert into vitess_test values(1, null, null, null)", nil)
	want := "Duplicate entry '1'"
	assert.ErrorContains(t, err, want)
	compareIntDiff(t, framework.DebugVars(), "Errors/ALREADY_EXISTS", vstart, 1)
}

func TestTrailingComment(t *testing.T) {
	v1 := framework.Server.QueryPlanCacheLen()

	bindVars := map[string]*querypb.BindVariable{"ival": sqltypes.Int64BindVariable(1)}
	client := framework.NewClient()

	for _, query := range []string{
		"select * from vitess_test where intval=:ival",
		"select * from vitess_test where intval=:ival /* comment */",
		"select * from vitess_test where intval=:ival /* comment1 */ /* comment2 */",
	} {
		_, err := client.Execute(query, bindVars)
		require.NoError(t, err)
		v2 := framework.Server.QueryPlanCacheLen()
		if v2 != v1+1 {
			t.Errorf("QueryEnginePlanCacheLength(%s): %d, want %d", query, v2, v1+1)
		}
	}
}

func TestSchemaReload(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &connParams)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecuteFetch("create table vitess_temp(intval int)", 10, false)
	require.NoError(t, err)
	defer conn.ExecuteFetch("drop table vitess_temp", 10, false)

	framework.Server.ReloadSchema(context.Background())
	client := framework.NewClient()
	waitTime := 50 * time.Millisecond
	for i := 0; i < 10; i++ {
		time.Sleep(waitTime)
		waitTime += 50 * time.Millisecond
		_, err = client.Execute("select * from vitess_temp", nil)
		if err == nil {
			return
		}
		want := "table vitess_temp not found in schema"
		if err.Error() != want {
			t.Errorf("Error: %v, want %s", err, want)
			return
		}
	}
	t.Error("schema did not reload")
}

func TestSidecarTables(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &connParams)
	require.NoError(t, err)
	defer conn.Close()
	for _, table := range []string{
		"redo_state",
		"redo_statement",
		"dt_state",
		"dt_participant",
	} {
		_, err = conn.ExecuteFetch(fmt.Sprintf("describe _vt.%s", table), 10, false)
		require.NoError(t, err)
	}
}

func TestConsolidation(t *testing.T) {
	defer framework.Server.SetPoolSize(context.Background(), framework.Server.PoolSize())

	err := framework.Server.SetPoolSize(context.Background(), 1)
	require.NoError(t, err)

	const tag = "Waits/Histograms/Consolidations/Count"

	for sleep := 0.1; sleep < 10.0; sleep *= 2 {
		want := framework.FetchInt(framework.DebugVars(), tag) + 1
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			query := fmt.Sprintf("/* query: 1 */ select sleep(%v) from dual /* query: 1 */", sleep)
			framework.NewClient().Execute(query, nil)
			wg.Done()
		}()
		go func() {
			query := fmt.Sprintf("/* query: 2 */ select sleep(%v) from dual /* query: 2 */", sleep)
			framework.NewClient().Execute(query, nil)
			wg.Done()
		}()
		wg.Wait()

		if framework.FetchInt(framework.DebugVars(), tag) == want {
			return
		}
		t.Logf("Consolidation didn't succeed with sleep for %v, trying a longer sleep", sleep)
	}
	t.Error("DebugVars for consolidation not incremented")
}

func TestBindInSelect(t *testing.T) {
	client := framework.NewClient()

	// Int bind var.
	qr, err := client.Execute(
		"select :bv from dual",
		map[string]*querypb.BindVariable{"bv": sqltypes.Int64BindVariable(1)},
	)
	require.NoError(t, err)
	want57 := &sqltypes.Result{
		Fields: []*querypb.Field{{
			Name:         "1",
			Type:         sqltypes.Int64,
			ColumnLength: 1,
			Charset:      63,
			Flags:        32897,
		}},
		Rows: [][]sqltypes.Value{
			{
				sqltypes.NewInt64(1),
			},
		},
	}
	want80 := want57.Copy()
	want80.Fields[0].ColumnLength = 2

	wantMaria := want57.Copy()
	wantMaria.Fields[0].Type = sqltypes.Int32
	wantMaria.Rows[0][0] = sqltypes.NewInt32(1)

	if !qr.Equal(want57) && !qr.Equal(want80) && !qr.Equal(wantMaria) {
		t.Errorf("Execute:\n%v, want\n%v,\n%v or\n%v", prettyPrint(*qr), prettyPrint(*want57), prettyPrint(*want80), prettyPrint(*wantMaria))
	}

	// String bind var.
	qr, err = client.Execute(
		"select :bv from dual",
		map[string]*querypb.BindVariable{"bv": sqltypes.StringBindVariable("abcd")},
	)
	require.NoError(t, err)
	want := &sqltypes.Result{
		Fields: []*querypb.Field{{
			Name:         "abcd",
			Type:         sqltypes.VarChar,
			ColumnLength: 16,
			Charset:      45,
			Flags:        1,
		}},
		Rows: [][]sqltypes.Value{
			{
				sqltypes.NewVarChar("abcd"),
			},
		},
	}
	// MariaDB 10.3 has different behavior.
	qr.Fields[0].Decimals = 0
	if !qr.Equal(want) {
		t.Errorf("Execute: \n%#v, want \n%#v", prettyPrint(*qr), prettyPrint(*want))
	}

	// Binary bind var.
	qr, err = client.Execute(
		"select :bv from dual",
		map[string]*querypb.BindVariable{"bv": sqltypes.StringBindVariable("\x00\xff")},
	)
	require.NoError(t, err)
	want = &sqltypes.Result{
		Fields: []*querypb.Field{{
			Name:         "",
			Type:         sqltypes.VarChar,
			ColumnLength: 8,
			Charset:      45,
			Flags:        1,
		}},
		Rows: [][]sqltypes.Value{
			{
				sqltypes.NewVarChar("\x00\xff"),
			},
		},
	}
	// MariaDB 10.3 has different behavior.
	qr.Fields[0].Decimals = 0
	if !qr.Equal(want) {
		t.Errorf("Execute: \n%#v, want \n%#v", prettyPrint(*qr), prettyPrint(*want))
	}
}

func TestHealth(t *testing.T) {
	response, err := http.Get(fmt.Sprintf("%s/debug/health", framework.ServerAddress))
	require.NoError(t, err)
	defer response.Body.Close()
	result, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if string(result) != "ok" {
		t.Errorf("Health check: %s, want ok", result)
	}
}

func TestStreamHealth(t *testing.T) {
	var health *querypb.StreamHealthResponse
	framework.Server.BroadcastHealth()
	if err := framework.Server.StreamHealth(context.Background(), func(shr *querypb.StreamHealthResponse) error {
		health = shr
		return io.EOF
	}); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(health.Target, framework.Target) {
		t.Errorf("Health: %+v, want %+v", health.Target, framework.Target)
	}
}

func TestQueryStats(t *testing.T) {
	client := framework.NewClient()
	vstart := framework.DebugVars()

	start := time.Now()
	query := "select /* query_stats */ eid from vitess_a where eid = :eid"
	bv := map[string]*querypb.BindVariable{"eid": sqltypes.Int64BindVariable(1)}
	if _, err := client.Execute(query, bv); err != nil {
		t.Fatal(err)
	}
	stat := framework.QueryStats()[query]
	duration := int(time.Since(start))
	if stat.Time <= 0 || stat.Time > duration {
		t.Errorf("stat.Time: %d, must be between 0 and %d", stat.Time, duration)
	}
	if stat.MysqlTime <= 0 || stat.MysqlTime > duration {
		t.Errorf("stat.MysqlTime: %d, must be between 0 and %d", stat.MysqlTime, duration)
	}
	stat.Time = 0
	stat.MysqlTime = 0
	want := framework.QueryStat{
		Query:        query,
		Table:        "vitess_a",
		Plan:         "Select",
		QueryCount:   1,
		RowsAffected: 0,
		RowsReturned: 2,
		ErrorCount:   0,
	}

	utils.MustMatch(t, want, stat)

	// Query cache should be updated for errors that happen at MySQL level also.
	query = "select /* query_stats */ eid from vitess_a where dontexist(eid) = :eid"
	_, _ = client.Execute(query, bv)
	stat = framework.QueryStats()[query]
	stat.Time = 0
	stat.MysqlTime = 0
	want = framework.QueryStat{
		Query:        query,
		Table:        "vitess_a",
		Plan:         "Select",
		QueryCount:   1,
		RowsAffected: 0,
		RowsReturned: 0,
		ErrorCount:   1,
	}
	utils.MustMatch(t, want, stat)
	vend := framework.DebugVars()
	require.False(t, framework.IsPresent(vend, "QueryRowsAffected/vitess_a.Select"))
	compareIntDiff(t, vend, "QueryCounts/vitess_a.Select", vstart, 2)
	compareIntDiff(t, vend, "QueryRowsReturned/vitess_a.Select", vstart, 2)
	compareIntDiff(t, vend, "QueryErrorCounts/vitess_a.Select", vstart, 1)
	compareIntDiff(t, vend, "QueryErrorCountsWithCode/vitess_a.Select.UNKNOWN", vstart, 1)

	query = "update /* query_stats */ vitess_a set name = 'a'"
	_, _ = client.Execute(query, bv)
	defer func() {
		// restore the table rows for other tests to use
		query = "update /* query_stats */ vitess_a set name = 'abcd' where id = 1"
		_, _ = client.Execute(query, bv)
		query = "update /* query_stats */ vitess_a set name = 'bcde' where id = 2"
		_, _ = client.Execute(query, bv)
	}()
	stat = framework.QueryStats()[query]
	stat.Time = 0
	stat.MysqlTime = 0
	want = framework.QueryStat{
		Query:        query,
		Table:        "vitess_a",
		Plan:         "UpdateLimit",
		QueryCount:   1,
		RowsAffected: 2,
		RowsReturned: 0,
		ErrorCount:   0,
	}
	utils.MustMatch(t, want, stat)
	vend = framework.DebugVars()
	require.False(t, framework.IsPresent(vend, "QueryRowsReturned/vitess_a.UpdateLimit"))
	compareIntDiff(t, vend, "QueryCounts/vitess_a.UpdateLimit", vstart, 1)
	compareIntDiff(t, vend, "QueryRowsAffected/vitess_a.UpdateLimit", vstart, 2)
	compareIntDiff(t, vend, "QueryErrorCounts/vitess_a.UpdateLimit", vstart, 0)

	query = "insert /* query_stats */ into vitess_a (eid, id, name, foo) values(100, 100, 'sdf', 'asdf')"
	_, _ = client.Execute(query, bv)
	stat = framework.QueryStats()[query]
	stat.Time = 0
	stat.MysqlTime = 0
	want = framework.QueryStat{
		Query:        query,
		Table:        "vitess_a",
		Plan:         "Insert",
		QueryCount:   1,
		RowsAffected: 1,
		RowsReturned: 0,
		ErrorCount:   0,
	}
	utils.MustMatch(t, want, stat)
	vend = framework.DebugVars()
	require.False(t, framework.IsPresent(vend, "QueryRowsReturned/vitess_a.Insert"))
	compareIntDiff(t, vend, "QueryCounts/vitess_a.Insert", vstart, 1)
	compareIntDiff(t, vend, "QueryRowsAffected/vitess_a.Insert", vstart, 1)
	compareIntDiff(t, vend, "QueryErrorCounts/vitess_a.Insert", vstart, 0)

	query = "delete /* query_stats */ from vitess_a where eid = 100"
	_, _ = client.Execute(query, bv)
	stat = framework.QueryStats()[query]
	stat.Time = 0
	stat.MysqlTime = 0
	want = framework.QueryStat{
		Query:        query,
		Table:        "vitess_a",
		Plan:         "DeleteLimit",
		QueryCount:   1,
		RowsAffected: 1,
		RowsReturned: 0,
		ErrorCount:   0,
	}
	utils.MustMatch(t, want, stat)
	vend = framework.DebugVars()
	require.False(t, framework.IsPresent(vend, "QueryRowsReturned/vitess_a.DeleteLimit"))
	compareIntDiff(t, vend, "QueryCounts/vitess_a.DeleteLimit", vstart, 1)
	compareIntDiff(t, vend, "QueryRowsAffected/vitess_a.DeleteLimit", vstart, 1)
	compareIntDiff(t, vend, "QueryErrorCounts/vitess_a.DeleteLimit", vstart, 0)

	// Ensure BeginExecute also updates the stats and strips comments.
	query = "select /* begin_execute */ 1 /* trailing comment */"
	if _, err := client.BeginExecute(query, bv, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, ok := framework.QueryStats()[query]; ok {
		t.Errorf("query stats included trailing comments for BeginExecute: %v", framework.QueryStats())
	}
	stripped := "select /* begin_execute */ 1"
	if _, ok := framework.QueryStats()[stripped]; !ok {
		t.Errorf("query stats did not get updated for BeginExecute: %v", framework.QueryStats())
	}
}

func TestDBAStatements(t *testing.T) {
	client := framework.NewClient()

	qr, err := client.Execute("show variables like 'version'", nil)
	require.NoError(t, err)
	wantCol := sqltypes.NewVarChar("version")
	if !reflect.DeepEqual(qr.Rows[0][0], wantCol) {
		t.Errorf("Execute: \n%#v, want \n%#v", qr.Rows[0][0], wantCol)
	}

	qr, err = client.Execute("describe vitess_a", nil)
	require.NoError(t, err)
	assert.Equal(t, 4, len(qr.Rows))

	qr, err = client.Execute("explain vitess_a", nil)
	require.NoError(t, err)
	assert.Equal(t, 4, len(qr.Rows))
}

type testLogger struct {
	logs        []string
	savedInfof  func(format string, args ...any)
	savedErrorf func(format string, args ...any)
}

func newTestLogger() *testLogger {
	tl := &testLogger{
		savedInfof:  log.Infof,
		savedErrorf: log.Errorf,
	}
	log.Infof = tl.recordInfof
	log.Errorf = tl.recordErrorf
	return tl
}

func (tl *testLogger) Close() {
	log.Infof = tl.savedInfof
	log.Errorf = tl.savedErrorf
}

func (tl *testLogger) recordInfof(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tl.logs = append(tl.logs, msg)
	tl.savedInfof(msg)
}

func (tl *testLogger) recordErrorf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	tl.logs = append(tl.logs, msg)
	tl.savedErrorf(msg)
}

func (tl *testLogger) getLog(i int) string {
	if i < len(tl.logs) {
		return tl.logs[i]
	}
	return fmt.Sprintf("ERROR: log %d/%d does not exist", i, len(tl.logs))
}

func TestClientFoundRows(t *testing.T) {
	client := framework.NewClient()
	if _, err := client.Execute("insert into vitess_test(intval, charval) values(124, 'aa')", nil); err != nil {
		t.Fatal(err)
	}
	defer client.Execute("delete from vitess_test where intval= 124", nil)

	// CLIENT_FOUND_ROWS flag is off.
	if err := client.Begin(false); err != nil {
		t.Error(err)
	}
	qr, err := client.Execute("update vitess_test set charval='aa' where intval=124", nil)
	require.NoError(t, err)
	assert.Equal(t, 0, len(qr.Rows))
	if err := client.Rollback(); err != nil {
		t.Error(err)
	}

	// CLIENT_FOUND_ROWS flag is on.
	if err := client.Begin(true); err != nil {
		t.Error(err)
	}
	qr, err = client.Execute("update vitess_test set charval='aa' where intval=124", nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, qr.RowsAffected)
	if err := client.Rollback(); err != nil {
		t.Error(err)
	}
}

func TestLastInsertId(t *testing.T) {
	client := framework.NewClient()
	_, err := client.Execute("insert ignore into vitess_autoinc_seq SET name = 'foo', sequence = 0", nil)
	require.NoError(t, err)
	defer client.Execute("delete from vitess_autoinc_seq where name = 'foo'", nil)
	err = client.Begin(true)
	require.NoError(t, err)
	defer client.Rollback()

	res, err := client.Execute("insert ignore into vitess_autoinc_seq SET name = 'foo', sequence = 0", nil)
	require.NoError(t, err)

	qr, err := client.Execute("update vitess_autoinc_seq set sequence=last_insert_id(sequence + 1) where name='foo'", nil)
	require.NoError(t, err)

	insID := res.InsertID
	assert.Equal(t, insID+1, qr.InsertID, "insertID")

	qr, err = client.Execute("select sequence from vitess_autoinc_seq where name = 'foo'", nil)
	require.NoError(t, err)

	wantCol := sqltypes.NewUint64(insID + uint64(1))
	assert.Truef(t, qr.Rows[0][0].Equal(wantCol), "Execute: \n%#v, want \n%#v", qr.Rows[0][0], wantCol)
}

func TestSelectLastInsertId(t *testing.T) {
	client := framework.NewClient()
	rs, err := client.ExecuteWithOptions("select 1 from dual where last_insert_id(42) = 42", nil, &querypb.ExecuteOptions{
		IncludedFields:    querypb.ExecuteOptions_ALL,
		FetchLastInsertId: true,
	})
	require.NoError(t, err)
	assert.EqualValues(t, 42, rs.InsertID)
}

func TestAppDebugRequest(t *testing.T) {
	client := framework.NewClient()

	// Insert with normal user works

	if _, err := client.Execute("insert into vitess_test_debuguser(intval, charval) values(124, 'aa')", nil); err != nil {
		t.Fatal(err)
	}

	defer client.Execute("delete from vitess_test where intval= 124", nil)

	// Set vt_appdebug
	ctx := callerid.NewContext(
		context.Background(),
		&vtrpcpb.CallerID{},
		&querypb.VTGateCallerID{Username: "vt_appdebug"})

	want := "Access denied for user 'vt_appdebug'@'localhost'"

	client = framework.NewClientWithContext(ctx)

	// Start a transaction. This test the other flow that a client can use to insert a value.
	client.Begin(false)
	_, err := client.Execute("insert into vitess_test_debuguser(intval, charval) values(124, 'aa')", nil)

	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Error: %v, want prefix %s", err, want)
	}

	// Normal flow, when a client is trying to insert a value and the insert is not in the
	// context of another transaction.
	_, err = client.Execute("insert into vitess_test_debuguser(intval, charval) values(124, 'aa')", nil)

	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Error: %v, want prefix %s", err, want)
	}

	_, err = client.Execute("select * from vitess_test_debuguser where intval=1", nil)
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("Error: %v, want prefix %s", err, want)
	}
}

func TestBeginExecuteWithFailingPreQueriesAndCheckConnectionState(t *testing.T) {
	client := framework.NewClient()

	insQuery := "insert into vitess_test (intval, floatval, charval, binval) values (4, null, null, null)"
	preQueries := []string{
		"savepoint a",
		"release savepoint b",
	}
	_, err := client.BeginExecute(insQuery, nil, preQueries)
	require.Error(t, err)

	qr, err := client.Execute("select intval from vitess_test where intval = 4", nil)
	require.NoError(t, err)
	require.Empty(t, qr.Rows)
}

func TestSelectBooleanSystemVariables(t *testing.T) {
	client := framework.NewClient()

	type testCase struct {
		Variable string
		Value    bool
		Type     querypb.Type
	}

	newTestCase := func(varname string, vartype querypb.Type, value bool) testCase {
		return testCase{Variable: varname, Value: value, Type: vartype}
	}

	tcs := []testCase{
		newTestCase("autocommit", querypb.Type_INT64, true),
		newTestCase("autocommit", querypb.Type_INT64, false),
		newTestCase("enable_system_settings", querypb.Type_INT64, true),
		newTestCase("enable_system_settings", querypb.Type_INT64, false),
	}

	for _, tc := range tcs {
		qr, err := client.Execute(
			fmt.Sprintf("select :%s", tc.Variable),
			map[string]*querypb.BindVariable{tc.Variable: sqltypes.BoolBindVariable(tc.Value)},
		)
		require.NoError(t, err)
		require.NotEmpty(t, qr.Fields, "fields should not be empty")
		require.Equal(t, tc.Type, qr.Fields[0].Type, fmt.Sprintf("invalid type, wants: %+v, but got: %+v\n", tc.Type, qr.Fields[0].Type))
	}
}

func TestSysSchema(t *testing.T) {
	client := framework.NewClient()
	_, err := client.Execute("drop table if exists `a`", nil)
	require.NoError(t, err)

	_, err = client.Execute("CREATE TABLE `a` (`one` int NOT NULL,`two` int NOT NULL,PRIMARY KEY (`one`,`two`)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4", nil)
	require.NoError(t, err)
	defer client.Execute("drop table `a`", nil)

	qr, err := client.Execute(`SELECT
		column_name column_name,
		data_type data_type,
		column_type full_data_type,
		character_maximum_length character_maximum_length,
		numeric_precision numeric_precision,
		numeric_scale numeric_scale,
		datetime_precision datetime_precision,
		column_default column_default,
		is_nullable is_nullable,
		extra extra,
		table_name table_name
	FROM information_schema.columns
	WHERE 1 != 1
	ORDER BY ordinal_position`, nil)
	require.NoError(t, err)

	// This is mysql behaviour that we are receiving Uint32 on field query even though the column is Uint64.
	// assert.EqualValues(t, sqltypes.Uint64, qr.Fields[4].Type) - ideally this should be received
	// The issue is only in MySQL 8.0 , As CI is on MySQL 5.7 need to check with Uint64
	assert.True(t, qr.Fields[4].Type == sqltypes.Uint64 || qr.Fields[4].Type == sqltypes.Uint32)

	qr, err = client.Execute(`SELECT
		column_name column_name,
		data_type data_type,
		column_type full_data_type,
		character_maximum_length character_maximum_length,
		numeric_precision numeric_precision,
		numeric_scale numeric_scale,
		datetime_precision datetime_precision,
		column_default column_default,
		is_nullable is_nullable,
		extra extra,
		table_name table_name
	FROM information_schema.columns
	WHERE table_schema = 'vttest' and table_name = 'a'
	ORDER BY ordinal_position`, nil)
	require.NoError(t, err)
	require.Equal(t, 2, len(qr.Rows))

	// is_nullable
	assert.Equal(t, `VARCHAR("NO")`, qr.Rows[0][8].String())
	assert.Equal(t, `VARCHAR("NO")`, qr.Rows[1][8].String())

	// table_name
	// This can be either a VARCHAR or a VARBINARY. On Linux and MySQL 8, the
	// string is tagged with a binary encoding, so it is VARBINARY.
	// On case-insensitive filesystems, it's a VARCHAR.
	assert.Contains(t, []string{`VARBINARY("a")`, `VARCHAR("a")`}, qr.Rows[0][10].String())
	assert.Contains(t, []string{`VARBINARY("a")`, `VARCHAR("a")`}, qr.Rows[1][10].String())

	// The field Type and the row value type are not matching and because of this wrong packet is send regarding the data of bigint unsigned to the client on vttestserver.
	// On, Vitess cluster using protobuf we are doing the row conversion to field type and so the final row type send to client is same as field type.
	// assert.EqualValues(t, sqltypes.Uint64, qr.Fields[4].Type) - We would have received this but because of field caching we are receiving Uint32.
	// The issue is only in MySQL 8.0 , As CI is on MySQL 5.7 need to check with Uint64
	assert.True(t, qr.Fields[4].Type == sqltypes.Uint64 || qr.Fields[4].Type == sqltypes.Uint32)
	assert.Equal(t, querypb.Type_UINT64, qr.Rows[0][4].Type())
}

func TestHexAndBitBindVar(t *testing.T) {
	client := framework.NewClient()

	bv := map[string]*querypb.BindVariable{
		"vtg1": sqltypes.HexNumBindVariable([]byte("0x9")),
		"vtg2": sqltypes.HexValBindVariable([]byte("X'09'")),
	}
	qr, err := client.Execute("select :vtg1, :vtg2, 0x9, X'09', 0b1001, B'1001'", bv)
	require.NoError(t, err)
	assert.Equal(t, `[[VARBINARY("\t") VARBINARY("\t") VARBINARY("\t") VARBINARY("\t") VARBINARY("\t") VARBINARY("\t")]]`, fmt.Sprintf("%v", qr.Rows))

	qr, err = client.Execute("select 1 + :vtg1, 1 + :vtg2, 1 + 0x9, 1 + X'09', 1 + 0b1001, 1 + B'1001'", bv)
	require.NoError(t, err)
	assert.Equal(t, `[[UINT64(10) UINT64(10) UINT64(10) UINT64(10) INT64(10) INT64(10)]]`, fmt.Sprintf("%v", qr.Rows))

	bv = map[string]*querypb.BindVariable{
		"vtg1": sqltypes.BitNumBindVariable([]byte("0b1001")),
		"vtg2": sqltypes.HexNumBindVariable([]byte("0x9")),
		"vtg3": sqltypes.BitNumBindVariable([]byte("0b100110101111")),
		"vtg4": sqltypes.HexNumBindVariable([]byte("0x9af")),
	}
	qr, err = client.Execute("select :vtg1, :vtg2, :vtg3, :vtg4", bv)
	require.NoError(t, err)
	assert.Equal(t, `[[VARBINARY("\t") VARBINARY("\t") VARBINARY("\t\xaf") VARBINARY("\t\xaf")]]`, fmt.Sprintf("%v", qr.Rows))

	qr, err = client.Execute("select 1 + :vtg1, 1 + :vtg2, 1 + :vtg3, 1 + :vtg4", bv)
	require.NoError(t, err)
	assert.Equal(t, `[[INT64(10) UINT64(10) INT64(2480) UINT64(2480)]]`, fmt.Sprintf("%v", qr.Rows))
}

// Test will validate drop view ddls.
func TestShowTablesWithSizes(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &connParams)
	require.NoError(t, err)
	defer conn.Close()

	if query := conn.BaseShowTablesWithSizes(); query == "" {
		// Happens in MySQL 8.0 where we use BaseShowInnodbTableSizes, instead.
		t.Skip("BaseShowTablesWithSizes is empty in this version of MySQL")
	}

	setupQueries := []string{
		`drop view if exists show_tables_with_sizes_v1`,
		`drop table if exists show_tables_with_sizes_t1`,
		`drop table if exists show_tables_with_sizes_employees`,
		`create table show_tables_with_sizes_t1 (id int primary key)`,
		`create view show_tables_with_sizes_v1 as select * from show_tables_with_sizes_t1`,
		`CREATE TABLE show_tables_with_sizes_employees (id INT NOT NULL, store_id INT) PARTITION BY HASH(store_id) PARTITIONS 4`,
		`create table show_tables_with_sizes_fts (id int primary key, name text, fulltext key name_fts (name))`,
	}

	defer func() {
		_, _ = conn.ExecuteFetch(`drop view if exists show_tables_with_sizes_v1`, 1, false)
		_, _ = conn.ExecuteFetch(`drop table if exists show_tables_with_sizes_t1`, 1, false)
		_, _ = conn.ExecuteFetch(`drop table if exists show_tables_with_sizes_employees`, 1, false)
		_, _ = conn.ExecuteFetch(`drop table if exists show_tables_with_sizes_fts`, 1, false)
	}()
	for _, query := range setupQueries {
		_, err := conn.ExecuteFetch(query, 1, false)
		require.NoError(t, err)
	}

	expectedTables := []string{
		"show_tables_with_sizes_t1",
		"show_tables_with_sizes_v1",
		"show_tables_with_sizes_employees",
		"show_tables_with_sizes_fts",
	}
	actualTables := []string{}

	rs, err := conn.ExecuteFetch(conn.BaseShowTablesWithSizes(), -1, false)
	require.NoError(t, err)
	require.NotEmpty(t, rs.Rows)

	assert.GreaterOrEqual(t, len(rs.Rows), len(expectedTables))

	for _, row := range rs.Rows {
		assert.Equal(t, 6, len(row))

		tableName := row[0].ToString()
		switch tableName {
		case "show_tables_with_sizes_t1":
			// TABLE_TYPE
			assert.Equal(t, "BASE TABLE", row[1].ToString())

			assert.True(t, row[2].IsIntegral())
			createTime, err := row[2].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, createTime)

			// TABLE_COMMENT
			assert.Equal(t, "", row[3].ToString())

			assert.True(t, row[4].IsDecimal())
			fileSize, err := row[4].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, fileSize)

			assert.True(t, row[4].IsDecimal())
			allocatedSize, err := row[5].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, allocatedSize)

			actualTables = append(actualTables, tableName)
		case "show_tables_with_sizes_v1":
			// TABLE_TYPE
			assert.Equal(t, "VIEW", row[1].ToString())

			assert.True(t, row[2].IsIntegral())
			createTime, err := row[2].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, createTime)

			// TABLE_COMMENT
			assert.Equal(t, "VIEW", row[3].ToString())

			assert.True(t, row[4].IsNull())
			assert.True(t, row[5].IsNull())

			actualTables = append(actualTables, tableName)
		case "show_tables_with_sizes_employees":
			// TABLE_TYPE
			assert.Equal(t, "BASE TABLE", row[1].ToString())

			assert.True(t, row[2].IsIntegral())
			createTime, err := row[2].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, createTime)

			// TABLE_COMMENT
			assert.Equal(t, "", row[3].ToString())

			assert.True(t, row[4].IsDecimal())
			fileSize, err := row[4].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, fileSize)

			assert.True(t, row[5].IsDecimal())
			allocatedSize, err := row[5].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, allocatedSize)

			actualTables = append(actualTables, tableName)
		case "show_tables_with_sizes_fts":
			// TABLE_TYPE
			assert.Equal(t, "BASE TABLE", row[1].ToString())

			assert.True(t, row[2].IsIntegral())
			createTime, err := row[2].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, createTime)

			// TABLE_COMMENT
			assert.Equal(t, "", row[3].ToString())

			assert.True(t, row[4].IsDecimal())
			fileSize, err := row[4].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, fileSize)

			assert.True(t, row[5].IsDecimal())
			allocatedSize, err := row[5].ToCastInt64()
			assert.NoError(t, err)
			assert.Positive(t, allocatedSize)

			actualTables = append(actualTables, tableName)
		}
	}

	assert.Equal(t, len(expectedTables), len(actualTables))
	assert.ElementsMatch(t, expectedTables, actualTables)
}

func newTestSchemaEngine(connParams *mysql.ConnParams) *schema.Engine {
	cfg := tabletenv.NewDefaultConfig()
	cfg.DB = dbconfigs.NewTestDBConfigs(*connParams, *connParams, connParams.DbName)
	env := tabletenv.NewEnv(vtenv.NewTestEnv(), cfg, "EngineTest")
	se := schema.NewEngine(env)
	se.InitDBConfig(dbconfigs.New(connParams))
	return se
}

func TestEngineReload(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &connParams)
	require.NoError(t, err)
	defer conn.Close()
	t.Run("validate innodb size query", func(t *testing.T) {
		q := conn.BaseShowInnodbTableSizes()
		require.NotEmpty(t, q)
	})
	t.Run("validate conn schema", func(t *testing.T) {
		rs, err := conn.ExecuteFetch(`select database() as d`, 1, true)
		require.NoError(t, err)
		row := rs.Named().Row()
		require.NotNil(t, row)
		database := row.AsString("d", "")
		require.Equal(t, connParams.DbName, database)
	})

	defer func() {
		_, _ = conn.ExecuteFetch(`drop view if exists view_simple`, 1, false)
		_, _ = conn.ExecuteFetch(`drop view if exists view_simple2`, 1, false)
		_, _ = conn.ExecuteFetch(`drop view if exists view_simple3`, 1, false)
		_, _ = conn.ExecuteFetch(`drop table if exists tbl_simple`, 1, false)
		_, _ = conn.ExecuteFetch(`drop table if exists tbl_part`, 1, false)
		_, _ = conn.ExecuteFetch(`drop table if exists tbl_fts`, 1, false)
	}()

	engine := newTestSchemaEngine(&connParams)
	require.NotNil(t, engine)
	err = engine.Open()
	require.NoError(t, err)
	defer engine.Close()

	t.Run("schema", func(t *testing.T) {
		setupQueries := []string{
			`drop view if exists view_simple`,
			`drop view if exists view_simple2`,
			`drop table if exists tbl_simple`,
			`drop table if exists tbl_nonpart`,
			`drop table if exists tbl_part`,
			`drop table if exists tbl_fts`,
			`create table tbl_simple (id int primary key)`,
			`create view view_simple as select * from tbl_simple`,
			`create view view_simple2 as select * from tbl_simple`,
			`create table tbl_nonpart (id INT NOT NULL, store_id INT)`,
			`create table tbl_part (id INT NOT NULL, store_id INT) PARTITION BY HASH(store_id) PARTITIONS 4`,
			`create table tbl_fts (id int primary key, name text, fulltext key name_fts (name))`,
		}

		for _, query := range setupQueries {
			_, err := conn.ExecuteFetch(query, 1, false)
			require.NoError(t, err)
		}

		expectedTables := []string{
			"tbl_simple",
			"tbl_nonpart",
			"tbl_part",
			"tbl_fts",
			"view_simple",
			"view_simple2",
		}
		err := engine.Reload(ctx)
		require.NoError(t, err)

		schema := engine.GetSchema()
		require.NotEmpty(t, schema)
		for _, expectTable := range expectedTables {
			t.Run(expectTable, func(t *testing.T) {
				tbl := engine.GetTable(sqlparser.NewIdentifierCS(expectTable))
				require.NotNil(t, tbl)

				switch expectTable {
				case "view_simple", "view_simple2":
					assert.Zero(t, tbl.FileSize)
					assert.Zero(t, tbl.AllocatedSize)
				default:
					assert.Zero(t, tbl.FileSize)
					assert.Zero(t, tbl.AllocatedSize)
				}
			})
		}
	})
	t.Run("schema changes", func(t *testing.T) {
		setupQueries := []string{
			`alter view view_simple as select *, 2 from tbl_simple`,
			`drop view view_simple2`,
			`create view view_simple3 as select * from tbl_simple`,
		}

		for _, query := range setupQueries {
			_, err := conn.ExecuteFetch(query, 1, false)
			require.NoError(t, err)
		}

		expectedTables := []string{
			"tbl_simple",
			"tbl_nonpart",
			"tbl_part",
			"tbl_fts",
			"view_simple",
			"view_simple3",
		}
		t.Run("reload without sizes", func(t *testing.T) {
			err := engine.Reload(ctx)
			require.NoError(t, err)

			schema := engine.GetSchema()
			require.NotEmpty(t, schema)
			for _, expectTable := range expectedTables {
				t.Run(expectTable, func(t *testing.T) {
					tbl := engine.GetTable(sqlparser.NewIdentifierCS(expectTable))
					require.NotNil(t, tbl)

					switch expectTable {
					case "view_simple", "view_simple2", "view_simple3":
						assert.Zero(t, tbl.FileSize)
						assert.Zero(t, tbl.AllocatedSize)
					default:
						assert.Zero(t, tbl.FileSize)
						assert.Zero(t, tbl.AllocatedSize)
					}
				})
			}
		})
		t.Run("reload with sizes", func(t *testing.T) {
			err := engine.ReloadAtEx(ctx, replication.Position{}, true)
			require.NoError(t, err)

			schema := engine.GetSchema()
			require.NotEmpty(t, schema)
			var nonPartitionedSize uint64
			var partitionedSize uint64
			for _, expectTable := range expectedTables {
				t.Run(expectTable, func(t *testing.T) {
					tbl := engine.GetTable(sqlparser.NewIdentifierCS(expectTable))
					require.NotNil(t, tbl)

					switch expectTable {
					case "view_simple", "view_simple2", "view_simple3":
						assert.Zero(t, tbl.FileSize)
						assert.Zero(t, tbl.AllocatedSize)
					case "tbl_nonpart":
						nonPartitionedSize = tbl.FileSize
						assert.Positive(t, tbl.FileSize)
						assert.Positive(t, tbl.AllocatedSize)
					case "tbl_part":
						partitionedSize = tbl.FileSize
						assert.Positive(t, tbl.FileSize)
						assert.Positive(t, tbl.AllocatedSize)
					default:
						assert.Positive(t, tbl.FileSize)
						assert.Positive(t, tbl.AllocatedSize)
					}
				})
			}
			assert.Positive(t, nonPartitionedSize)
			assert.Positive(t, partitionedSize)
			// "tbl_part" has 4 partitions (each of which has about the same size as "tbl_nonpart")
			// Technically partitionedSize should be 4*nonPartitionedSize, but we allow for some variance
			assert.Greater(t, partitionedSize, nonPartitionedSize)
			assert.Greater(t, partitionedSize, 3*nonPartitionedSize)
			assert.Less(t, partitionedSize, 5*nonPartitionedSize)
		})
	})
}

func TestUpdateTableIndexMetrics(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &connParams)
	require.NoError(t, err)
	defer conn.Close()

	if query := conn.BaseShowInnodbTableSizes(); query == "" {
		t.Skip("additional table/index metrics not updated in this version of MySQL")
	}
	client := framework.NewClient()

	_, err = client.Execute("insert into vitess_part (id) values (5),(15),(25)", nil)
	require.NoError(t, err)
	defer client.Execute("delete from vitess_part where id in (5,15,25)", nil)

	// Analyze tables to make sure stats are updated prior to reload
	tables := []string{"vitess_a", "vitess_part", "vitess_autoinc_seq"}
	for _, table := range tables {
		_, err = client.Execute(fmt.Sprintf("analyze table %s", table), nil)
		require.NoError(t, err)
	}

	// Wait up to 5s for the rows added to vitess_part to be reflected in DebugVars
	updated := false
	for i := 0; !updated && i < 10; i++ {
		err = framework.Server.ReloadSchema(ctx)
		require.NoError(t, err)

		if framework.FetchVal(framework.DebugVars(), "TableRows/vitess_part") == 3 {
			updated = true
		} else {
			time.Sleep(500 * time.Millisecond)
		}
	}

	results, err := client.Execute("select @@innodb_page_size", nil)
	require.NoError(t, err)
	pageSize, err := results.Rows[0][0].ToFloat64()
	require.NoError(t, err)

	vars := framework.DebugVars()

	assert.Equal(t, 2.0, framework.FetchVal(vars, "TableRows/vitess_a"))
	assert.Equal(t, 3.0, framework.FetchVal(vars, "TableRows/vitess_part"))
	partTableCountResult, _ := client.Execute("select count(1) from vitess_part", nil)
	partTableRows, _ := partTableCountResult.Rows[0][0].ToInt()
	assert.Equal(t, 3, partTableRows)

	assert.Equal(t, pageSize, framework.FetchVal(vars, "TableClusteredIndexSize/vitess_a"))
	assert.Equal(t, pageSize*2, framework.FetchVal(vars, "TableClusteredIndexSize/vitess_part"))

	assert.Equal(t, 2.0, framework.FetchVal(vars, "IndexCardinality/vitess_a.PRIMARY"))
	assert.Equal(t, 3.0, framework.FetchVal(vars, "IndexCardinality/vitess_part.PRIMARY"))
	assert.Equal(t, 0.0, framework.FetchVal(vars, "IndexCardinality/vitess_autoinc_seq.name"))

	assert.Equal(t, pageSize, framework.FetchVal(vars, "IndexBytes/vitess_a.PRIMARY"))
	assert.Equal(t, pageSize*2, framework.FetchVal(vars, "IndexBytes/vitess_part.PRIMARY"))
	assert.Equal(t, pageSize, framework.FetchVal(vars, "IndexBytes/vitess_autoinc_seq.name"))
}

// TestTuple tests that bind variables having tuple values work with vttablet.
func TestTuple(t *testing.T) {
	client := framework.NewClient()
	_, err := client.Execute(`insert into vitess_a (eid, id) values (100, 103), (193, 235)`, nil)
	require.NoError(t, err)

	bv := map[string]*querypb.BindVariable{
		"__vals": {
			Type: querypb.Type_TUPLE,
			Values: []*querypb.Value{
				sqltypes.TupleToProto([]sqltypes.Value{sqltypes.NewInt64(100), sqltypes.NewInt64(103)}),
				sqltypes.TupleToProto([]sqltypes.Value{sqltypes.NewInt64(87), sqltypes.NewInt64(4473)}),
			},
		},
	}
	res, err := client.Execute("select * from vitess_a where (eid, id) in ::__vals", bv)
	require.NoError(t, err)
	assert.Equal(t, `[[INT64(100) INT32(103) NULL NULL]]`, fmt.Sprintf("%v", res.Rows))

	res, err = client.Execute("update vitess_a set name = 'a' where (eid, id) in ::__vals", bv)
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.RowsAffected)

	res, err = client.Execute("select * from vitess_a where (eid, id) in ::__vals", bv)
	require.NoError(t, err)
	assert.Equal(t, `[[INT64(100) INT32(103) VARCHAR("a") NULL]]`, fmt.Sprintf("%v", res.Rows))

	bv = map[string]*querypb.BindVariable{
		"__vals": {
			Type: querypb.Type_TUPLE,
			Values: []*querypb.Value{
				sqltypes.TupleToProto([]sqltypes.Value{sqltypes.NewInt64(100), sqltypes.NewInt64(103)}),
				sqltypes.TupleToProto([]sqltypes.Value{sqltypes.NewInt64(193), sqltypes.NewInt64(235)}),
			},
		},
	}
	res, err = client.Execute("delete from vitess_a where (eid, id) in ::__vals", bv)
	require.NoError(t, err)
	assert.EqualValues(t, 2, res.RowsAffected)

	res, err = client.Execute("select * from vitess_a where (eid, id) in ::__vals", bv)
	require.NoError(t, err)
	require.Zero(t, len(res.Rows))
}

// TestMaxRows tests different scenarios with max rows.
func TestMaxRows(t *testing.T) {
	oldPT := framework.Server.Config().PassthroughDML
	oldMR := framework.Server.MaxResultSize()
	defer func() {
		framework.Server.SetPassthroughDMLs(oldPT)
		framework.Server.SetMaxResultSize(oldMR)
	}()

	client := framework.NewClient()

	_, err := client.Execute(`insert into maxrows_tbl (id, col) values (100, 200), (300, 400)`, nil)
	require.NoError(t, err)

	framework.Server.SetMaxResultSize(1)
	_, err = client.Execute(`select * from maxrows_tbl`, nil)
	require.ErrorContains(t, err, "Row count exceeded 1")

	// setting passthrough dml to true
	framework.Server.Config().PassthroughDML = true

	// this should still fail as InDMLExecution should be true as well.
	_, err = client.Execute(`select * from maxrows_tbl`, nil)
	require.ErrorContains(t, err, "Row count exceeded 1")

	// setting InDMLExecution to true
	inDMLExecOption := &querypb.ExecuteOptions{InDmlExecution: true}

	// this should still fail as it only works inside a transaction
	_, err = client.ExecuteWithOptions(`select * from maxrows_tbl`, nil, inDMLExecOption)
	require.ErrorContains(t, err, "[BUG] SelectNoLimit unexpected plan type", "this is expected only inside a transaction")

	// this should work as it is inside a transaction.
	require.NoError(t,
		client.Begin(false))
	_, err = client.ExecuteWithOptions(`select * from maxrows_tbl`, nil, inDMLExecOption)
	require.NoError(t, err, "Passthrough DML with In DML Execution should not be affected by max rows")
	require.NoError(t,
		client.Commit())
}
