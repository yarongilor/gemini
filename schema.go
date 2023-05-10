// Copyright 2019 ScyllaDB
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package gemini

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/pkg/errors"
	"github.com/scylladb/gocqlx/v2/qb"
	"golang.org/x/exp/rand"

	"github.com/scylladb/gemini/replication"
	"github.com/scylladb/gemini/tableopts"
)

type CQLFeature int

const (
	CQL_FEATURE_BASIC CQLFeature = iota + 1
	CQL_FEATURE_NORMAL
	CQL_FEATURE_ALL

	KnownIssuesJSONWithTuples = "https://github.com/scylladb/scylla/issues/3708"
)

type Value []interface{}

func (v Value) Copy() Value {
	values := make(Value, len(v))
	copy(values, v)
	return values
}

type SchemaConfig struct {
	ReplicationStrategy              *replication.Replication
	OracleReplicationStrategy        *replication.Replication
	TableOptions                     []tableopts.Option
	MaxTables                        int
	MaxPartitionKeys                 int
	MinPartitionKeys                 int
	MaxClusteringKeys                int
	MinClusteringKeys                int
	MaxColumns                       int
	MinColumns                       int
	MaxUDTParts                      int
	MaxTupleParts                    int
	MaxBlobLength                    int
	MaxStringLength                  int
	MinBlobLength                    int
	MinStringLength                  int
	UseCounters                      bool
	UseLWT                           bool
	CQLFeature                       CQLFeature
	AsyncObjectStabilizationAttempts int
	AsyncObjectStabilizationDelay    time.Duration
}

var (
	ErrSchemaConfigInvalidPK   = errors.New("max number of partition keys must be bigger than min number of partition keys")
	ErrSchemaConfigInvalidCK   = errors.New("max number of clustering keys must be bigger than min number of clustering keys")
	ErrSchemaConfigInvalidCols = errors.New("max number of columns must be bigger than min number of columns")
)

func (sc *SchemaConfig) Valid() error {
	if sc.MaxPartitionKeys <= sc.MinPartitionKeys {
		return ErrSchemaConfigInvalidPK
	}
	if sc.MaxClusteringKeys <= sc.MinClusteringKeys {
		return ErrSchemaConfigInvalidCK
	}
	if sc.MaxColumns <= sc.MinClusteringKeys {
		return ErrSchemaConfigInvalidCols
	}
	return nil
}

func (sc *SchemaConfig) GetMaxTables() int {
	return sc.MaxTables
}

func (sc *SchemaConfig) GetMaxPartitionKeys() int {
	return sc.MaxPartitionKeys
}

func (sc *SchemaConfig) GetMinPartitionKeys() int {
	return sc.MinPartitionKeys
}

func (sc *SchemaConfig) GetMaxClusteringKeys() int {
	return sc.MaxClusteringKeys
}

func (sc *SchemaConfig) GetMinClusteringKeys() int {
	return sc.MinClusteringKeys
}

func (sc *SchemaConfig) GetMaxColumns() int {
	return sc.MaxColumns
}

func (sc *SchemaConfig) GetMinColumns() int {
	return sc.MinColumns
}

type Keyspace struct {
	Replication       *replication.Replication `json:"replication"`
	OracleReplication *replication.Replication `json:"oracle_replication"`
	Name              string                   `json:"name"`
}

type Type interface {
	Name() string
	CQLDef() string
	CQLHolder() string
	CQLPretty(string, []interface{}) (string, int)
	GenValue(*rand.Rand, *PartitionRangeConfig) []interface{}
	Indexable() bool
	CQLType() gocql.TypeInfo
}

type IndexDef struct {
	Name      string `json:"name"`
	Column    string `json:"column"`
	ColumnIdx int    `json:"column_idx"`
}

type Table struct {
	Name              string             `json:"name"`
	PartitionKeys     Columns            `json:"partition_keys"`
	ClusteringKeys    Columns            `json:"clustering_keys"`
	Columns           Columns            `json:"columns"`
	Indexes           []IndexDef         `json:"indexes,omitempty"`
	MaterializedViews []MaterializedView `json:"materialized_views,omitempty"`
	KnownIssues       map[string]bool    `json:"known_issues"`
	TableOptions      []string           `json:"table_options,omitempty"`

	// mu protects the table during schema changes
	mu sync.RWMutex
}

func (t *Table) Lock() {
	t.mu.Lock()
}

func (t *Table) Unlock() {
	t.mu.Unlock()
}

func (t *Table) GetCreateTable(ks Keyspace) string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var (
		partitionKeys  []string
		clusteringKeys []string
		columns        []string
	)
	for _, pk := range t.PartitionKeys {
		partitionKeys = append(partitionKeys, pk.Name)
		columns = append(columns, fmt.Sprintf("%s %s", pk.Name, pk.Type.CQLDef()))
	}
	for _, ck := range t.ClusteringKeys {
		clusteringKeys = append(clusteringKeys, ck.Name)
		columns = append(columns, fmt.Sprintf("%s %s", ck.Name, ck.Type.CQLDef()))
	}
	for _, cdef := range t.Columns {
		columns = append(columns, fmt.Sprintf("%s %s", cdef.Name, cdef.Type.CQLDef()))
	}

	var stmt string
	if len(clusteringKeys) == 0 {
		stmt = fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.%s (%s, PRIMARY KEY ((%s)))", ks.Name, t.Name, strings.Join(columns, ","), strings.Join(partitionKeys, ","))
	} else {
		stmt = fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.%s (%s, PRIMARY KEY ((%s), %s))", ks.Name, t.Name, strings.Join(columns, ","),
			strings.Join(partitionKeys, ","), strings.Join(clusteringKeys, ","))
	}
	/*
		if t.CompactionStrategy != nil {
			stmt = stmt + " WITH compaction = " + t.CompactionStrategy.ToCQL() + ";"
		}*/
	if len(t.TableOptions) > 0 {
		stmt = stmt + " WITH " + strings.Join(t.TableOptions, " AND ") + ";"
	}
	return stmt
}

func (t *Table) GetCreateTypes(keyspace Keyspace) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var stmts []string
	for _, column := range t.Columns {
		switch c := column.Type.(type) {
		case *UDTType:
			createType := "CREATE TYPE IF NOT EXISTS %s.%s (%s)"
			var typs []string
			for name, typ := range c.Types {
				typs = append(typs, name+" "+typ.CQLDef())
			}
			stmts = append(stmts, fmt.Sprintf(createType, keyspace.Name, c.TypeName, strings.Join(typs, ",")))
		}
	}
	return stmts
}

type AlterTableBuilder struct {
	stmt string
}

func (atb *AlterTableBuilder) ToCql() (string, []string) {
	return atb.stmt, nil
}

func (t *Table) addColumn(keyspace string, sc *SchemaConfig) ([]*Stmt, func(), error) {
	var stmts []*Stmt
	column := ColumnDef{Name: genColumnName("col", len(t.Columns)+1), Type: genColumnType(len(t.Columns)+1, sc)}
	if c, ok := column.Type.(*UDTType); ok {
		createType := "CREATE TYPE IF NOT EXISTS %s.%s (%s);"
		var typs []string
		for name, typ := range c.Types {
			typs = append(typs, name+" "+typ.CQLDef())
		}
		stmt := fmt.Sprintf(createType, keyspace, c.TypeName, strings.Join(typs, ","))
		stmts = append(stmts, &Stmt{
			Query: &AlterTableBuilder{
				stmt: stmt,
			},
		})
	}
	stmt := "ALTER TABLE " + keyspace + "." + t.Name + " ADD " + column.Name + " " + column.Type.CQLDef()
	stmts = append(stmts, &Stmt{
		Query: &AlterTableBuilder{
			stmt: stmt,
		},
	})
	return stmts, func() {
		t.Columns = append(t.Columns, &column)
	}, nil
}

//nolint:unused
func (t *Table) alterColumn(keyspace string) ([]*Stmt, func(), error) {
	var stmts []*Stmt
	idx := rand.Intn(len(t.Columns))
	column := t.Columns[idx]
	oldType, isSimpleType := column.Type.(SimpleType)
	if !isSimpleType {
		return nil, func() {}, errors.Errorf("complex type=%s cannot be altered", column.Name)
	}
	if compatTypes, ok := compatibleColumnTypes[oldType]; ok {
		newType := compatTypes[rand.Intn(len(compatTypes))]
		newColumn := ColumnDef{Name: column.Name, Type: newType}
		stmt := "ALTER TABLE " + keyspace + "." + t.Name + " ALTER " + column.Name + " TYPE " + column.Type.CQLDef()
		stmts = append(stmts, &Stmt{
			Query: &AlterTableBuilder{
				stmt: stmt,
			},
			QueryType: AlterColumnStatementType,
		})
		return stmts, func() {
			t.Columns[idx] = &newColumn
		}, nil
	}
	return nil, func() {}, errors.Errorf("simple type=%s has no compatible types so it cannot be altered", column.Name)
}

func (t *Table) dropColumn(keyspace string) ([]*Stmt, func(), error) {
	var stmts []*Stmt
	idx := rand.Intn(len(t.Columns))
	column := t.Columns[idx]
	stmt := "ALTER TABLE " + keyspace + "." + t.Name + " DROP " + column.Name
	stmts = append(stmts, &Stmt{
		Query: &AlterTableBuilder{
			stmt: stmt,
		},
		QueryType: DropColumnStatementType,
	})
	return stmts, func() {
		t.Columns = append(t.Columns[:idx], t.Columns[idx+1:]...)
	}, nil
}

type MaterializedView struct {
	NonPrimaryKey  ColumnDef
	Name           string  `json:"name"`
	PartitionKeys  Columns `json:"partition_keys"`
	ClusteringKeys Columns `json:"clustering_keys"`
}

type Stmt struct {
	ValuesWithToken *ValueWithToken
	Query           qb.Builder
	Values          []interface{}
	Types           []Type
	QueryType       StatementType
}

type StatementType uint8

func (st StatementType) PossibleAsyncOperation() bool {
	return st == SelectByIndexStatementType || st == SelectFromMaterializedViewStatementType
}

const (
	SelectStatementType StatementType = iota
	SelectRangeStatementType
	SelectByIndexStatementType
	SelectFromMaterializedViewStatementType
	DeleteStatementType
	InsertStatement
	Updatetatement
	AlterColumnStatementType
	DropColumnStatementType
)

func (s *Stmt) PrettyCQL() string {
	var replaced int
	query, _ := s.Query.ToCql()
	values := s.Values
	if len(values) == 0 {
		return query
	}
	for _, typ := range s.Types {
		query, replaced = typ.CQLPretty(query, values)
		if len(values) >= replaced {
			values = values[replaced:]
		} else {
			break
		}
	}
	return query
}

type Schema struct {
	Keyspace Keyspace `json:"keyspace"`
	Tables   []*Table `json:"tables"`
}

type PartitionRangeConfig struct {
	MaxBlobLength   int
	MinBlobLength   int
	MaxStringLength int
	MinStringLength int
	UseLWT          bool
}

func (s *Schema) GetDropSchema() []string {
	return []string{
		fmt.Sprintf("DROP KEYSPACE IF EXISTS %s", s.Keyspace.Name),
	}
}

func GenSchema(sc SchemaConfig) *Schema {
	builder := NewSchemaBuilder()
	keyspace := Keyspace{
		Name:              "ks1",
		Replication:       sc.ReplicationStrategy,
		OracleReplication: sc.OracleReplicationStrategy,
	}
	builder.Keyspace(keyspace)
	numTables := 1 + rand.Intn(sc.GetMaxTables())
	for i := 0; i < numTables; i++ {
		table := createTable(sc, fmt.Sprintf("table%d", i+1))
		builder.Table(table)
	}
	return builder.Build()
}

func createTable(sc SchemaConfig, tableName string) *Table {
	numPartitionKeys := rand.Intn(sc.GetMaxPartitionKeys()-sc.GetMinPartitionKeys()) + sc.GetMinPartitionKeys()
	partitionKeys := make(Columns, numPartitionKeys)
	for i := 0; i < numPartitionKeys; i++ {
		partitionKeys[i] = &ColumnDef{Name: genColumnName("pk", i), Type: genPartitionKeyColumnType()}
	}
	numClusteringKeys := rand.Intn(sc.GetMaxClusteringKeys()-sc.GetMinClusteringKeys()) + sc.GetMinClusteringKeys()
	clusteringKeys := make(Columns, numClusteringKeys)
	for i := 0; i < numClusteringKeys; i++ {
		clusteringKeys[i] = &ColumnDef{Name: genColumnName("ck", i), Type: genPrimaryKeyColumnType()}
	}
	table := Table{
		Name:           tableName,
		PartitionKeys:  partitionKeys,
		ClusteringKeys: clusteringKeys,
		KnownIssues: map[string]bool{
			KnownIssuesJSONWithTuples: true,
		},
	}
	for _, option := range sc.TableOptions {
		table.TableOptions = append(table.TableOptions, option.ToCQL())
	}
	if sc.UseCounters {
		columns := Columns{
			{
				Name: genColumnName("col", 0),
				Type: &CounterType{
					Value: 0,
				},
			},
		}
		table.Columns = columns
	} else {
		var columns Columns
		numColumns := rand.Intn(sc.GetMaxColumns()-sc.GetMinColumns()) + sc.GetMinColumns()
		for i := 0; i < numColumns; i++ {
			columns = append(columns, &ColumnDef{Name: genColumnName("col", i), Type: genColumnType(numColumns, &sc)})
		}
		var indexes []IndexDef
		if sc.CQLFeature > CQL_FEATURE_BASIC && numColumns > 0 {
			indexes = columns.CreateIndexes(tableName, 1+rand.Intn(numColumns-1))
		}

		var mvs []MaterializedView
		if sc.CQLFeature > CQL_FEATURE_BASIC && numClusteringKeys > 0 {
			mvs = columns.CreateMaterializedViews(table.Name, partitionKeys, clusteringKeys)
		}

		table.Columns = columns
		table.MaterializedViews = mvs
		table.Indexes = indexes
	}
	return &table
}

func (s *Schema) GetCreateKeyspaces() (string, string) {
	return fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH REPLICATION = %s", s.Keyspace.Name, s.Keyspace.Replication.ToCQL()),
		fmt.Sprintf("CREATE KEYSPACE IF NOT EXISTS %s WITH REPLICATION = %s", s.Keyspace.Name, s.Keyspace.OracleReplication.ToCQL())
}

func (s *Schema) GetCreateSchema() []string {
	var stmts []string

	for _, t := range s.Tables {
		createTypes := t.GetCreateTypes(s.Keyspace)
		stmts = append(stmts, createTypes...)
		createTable := t.GetCreateTable(s.Keyspace)
		stmts = append(stmts, createTable)
		for _, idef := range t.Indexes {
			stmts = append(stmts, fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s.%s (%s)", idef.Name, s.Keyspace.Name, t.Name, idef.Column))
		}
		for _, mv := range t.MaterializedViews {
			var (
				mvPartitionKeys      []string
				mvPrimaryKeysNotNull []string
			)
			for _, pk := range mv.PartitionKeys {
				mvPartitionKeys = append(mvPartitionKeys, pk.Name)
				mvPrimaryKeysNotNull = append(mvPrimaryKeysNotNull, fmt.Sprintf("%s IS NOT NULL", pk.Name))
			}
			for _, ck := range mv.ClusteringKeys {
				mvPrimaryKeysNotNull = append(mvPrimaryKeysNotNull, fmt.Sprintf("%s IS NOT NULL", ck.Name))
			}
			var createMaterializedView string
			if len(mv.PartitionKeys) == 1 {
				createMaterializedView = "CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s AS SELECT * FROM %s.%s WHERE %s PRIMARY KEY (%s"
			} else {
				createMaterializedView = "CREATE MATERIALIZED VIEW IF NOT EXISTS %s.%s AS SELECT * FROM %s.%s WHERE %s PRIMARY KEY ((%s)"
			}
			createMaterializedView = createMaterializedView + ",%s)"
			stmts = append(stmts, fmt.Sprintf(createMaterializedView,
				s.Keyspace.Name, mv.Name, s.Keyspace.Name, t.Name,
				strings.Join(mvPrimaryKeysNotNull, " AND "),
				strings.Join(mvPartitionKeys, ","), strings.Join(t.ClusteringKeys.Names(), ",")))
		}
	}
	return stmts
}

func (s *Schema) genInsertStmt(t *Table, valuesWithToken *ValueWithToken, r *rand.Rand, p *PartitionRangeConfig) (*Stmt, error) {
	if isCounterTable(t) {
		return s.updateStmt(t, valuesWithToken, r, p)
	}
	return s.insertStmt(t, valuesWithToken, r, p)
}

func (s *Schema) updateStmt(t *Table, valuesWithToken *ValueWithToken, r *rand.Rand, p *PartitionRangeConfig) (*Stmt, error) {
	var typs []Type
	builder := qb.Update(s.Keyspace.Name + "." + t.Name)
	for _, pk := range t.PartitionKeys {
		builder = builder.Where(qb.Eq(pk.Name))
		typs = append(typs, pk.Type)
	}
	values := valuesWithToken.Value.Copy()
	for _, ck := range t.ClusteringKeys {
		builder = builder.Where(qb.Eq(ck.Name))
		values = appendValue(ck.Type, r, p, values)
		typs = append(typs, ck.Type)
	}
	var (
		colValues Value
		colTyps   []Type
	)
	for _, cdef := range t.Columns {
		switch t := cdef.Type.(type) {
		case *TupleType:
			builder = builder.SetTuple(cdef.Name, len(t.Types))
		case *CounterType:
			builder = builder.SetLit(cdef.Name, cdef.Name+"+1")
			continue
		default:
			builder = builder.Set(cdef.Name)
		}
		colValues = appendValue(cdef.Type, r, p, colValues)
		colTyps = append(colTyps, cdef.Type)
	}
	return &Stmt{
		ValuesWithToken: valuesWithToken,
		Query:           builder,
		Values:          append(colValues, values...),
		Types:           append(colTyps, typs...),
		QueryType:       Updatetatement,
	}, nil
}

func (s *Schema) insertStmt(t *Table, valuesWithToken *ValueWithToken, r *rand.Rand, p *PartitionRangeConfig) (*Stmt, error) {
	var typs []Type
	builder := qb.Insert(s.Keyspace.Name + "." + t.Name)
	for _, pk := range t.PartitionKeys {
		builder = builder.Columns(pk.Name)
		typs = append(typs, pk.Type)
	}
	values := valuesWithToken.Value.Copy()
	for _, ck := range t.ClusteringKeys {
		builder = builder.Columns(ck.Name)
		values = appendValue(ck.Type, r, p, values)
		typs = append(typs, ck.Type)
	}
	for _, cdef := range t.Columns {
		switch t := cdef.Type.(type) {
		case *TupleType:
			builder = builder.TupleColumn(cdef.Name, len(t.Types))
		default:
			builder = builder.Columns(cdef.Name)
		}
		values = appendValue(cdef.Type, r, p, values)
		typs = append(typs, cdef.Type)
	}
	if p.UseLWT && r.Uint32()%10 == 0 {
		builder = builder.Unique()
	}
	return &Stmt{
		ValuesWithToken: valuesWithToken,
		Query:           builder,
		Values:          values,
		Types:           typs,
		QueryType:       InsertStatement,
	}, nil
}

func (s *Schema) genInsertJSONStmt(table *Table, valuesWithToken *ValueWithToken, r *rand.Rand, p *PartitionRangeConfig) (*Stmt, error) {
	var v string
	var ok bool
	if isCounterTable(table) {
		return nil, nil
	}
	vs := valuesWithToken.Value.Copy()
	values := make(map[string]interface{})
	for i, pk := range table.PartitionKeys {
		switch t := pk.Type.(type) {
		case SimpleType:
			if t != TYPE_BLOB {
				values[pk.Name] = vs[i]
				continue
			}
			v, ok = vs[i].(string)
			if ok {
				values[pk.Name] = "0x" + v
			}
		case *TupleType:
			tupVals := make([]interface{}, len(t.Types))
			for j := 0; j < len(t.Types); j++ {
				if t.Types[j] == TYPE_BLOB {
					v, ok = vs[i+j].(string)
					if ok {
						v = "0x" + v
					}
					vs[i+j] = v
				}
				tupVals[i] = vs[i+j]
				i++
			}
			values[pk.Name] = tupVals
		default:
			panic(fmt.Sprintf("unknown type: %s", t.Name()))
		}
	}
	values = table.ClusteringKeys.ToJSONMap(values, r, p)
	values = table.Columns.ToJSONMap(values, r, p)

	jsonString, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}

	builder := qb.Insert(s.Keyspace.Name + "." + table.Name).Json()
	return &Stmt{
		ValuesWithToken: valuesWithToken,
		Query:           builder,
		Values:          []interface{}{string(jsonString)},
		Types:           []Type{TYPE_TEXT},
		QueryType:       InsertStatement,
	}, nil
}

func (s *Schema) genDeleteRows(t *Table, valuesWithToken *ValueWithToken, r *rand.Rand, p *PartitionRangeConfig) (*Stmt, error) {
	var typs []Type
	builder := qb.Delete(s.Keyspace.Name + "." + t.Name)
	for _, pk := range t.PartitionKeys {
		builder = builder.Where(qb.Eq(pk.Name))
		typs = append(typs, pk.Type)
	}

	values := valuesWithToken.Value.Copy()
	if len(t.ClusteringKeys) > 0 {
		ck := t.ClusteringKeys[0]
		builder = builder.Where(qb.GtOrEq(ck.Name)).Where(qb.LtOrEq(ck.Name))
		values = appendValue(ck.Type, r, p, values)
		values = appendValue(ck.Type, r, p, values)
		typs = append(typs, ck.Type, ck.Type)
	}
	return &Stmt{
		ValuesWithToken: valuesWithToken,
		Query:           builder,
		Values:          values,
		Types:           typs,
		QueryType:       DeleteStatementType,
	}, nil
}

func (s *Schema) GenDDLStmt(t *Table, r *rand.Rand, p *PartitionRangeConfig, sc *SchemaConfig) ([]*Stmt, func(), error) {
	switch n := r.Intn(3); n {
	// case 0: // Alter column not supported in Cassandra from 3.0.11
	//	return t.alterColumn(s.Keyspace.Name)
	case 1: // Delete column
		return t.dropColumn(s.Keyspace.Name)
	default: // Alter column
		return t.addColumn(s.Keyspace.Name, sc)
	}
}

func (s *Schema) GenMutateStmt(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig, deletes bool) (*Stmt, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	valuesWithToken := g.Get()
	if valuesWithToken == nil {
		return nil, nil
	}

	if !deletes {
		return s.genInsertStmt(t, valuesWithToken, r, p)
	}
	switch n := rand.Intn(1000); n {
	case 10, 100:
		return s.genDeleteRows(t, valuesWithToken, r, p)
	default:
		switch rand.Intn(2) {
		case 0:
			if t.KnownIssues[KnownIssuesJSONWithTuples] {
				return s.genInsertStmt(t, valuesWithToken, r, p)
			}
			return s.genInsertJSONStmt(t, valuesWithToken, r, p)
		default:
			return s.genInsertStmt(t, valuesWithToken, r, p)
		}
	}
}

func (s *Schema) GenCheckStmt(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig) *Stmt {
	var n int
	if len(t.Indexes) > 0 {
		n = r.Intn(5)
	} else {
		n = r.Intn(4)
	}
	switch n {
	case 0:
		return s.genSinglePartitionQuery(t, g, r, p)
	case 1:
		return s.genMultiplePartitionQuery(t, g, r, p)
	case 2:
		return s.genClusteringRangeQuery(t, g, r, p)
	case 3:
		return s.genMultiplePartitionClusteringRangeQuery(t, g, r, p)
	case 4:
		// Reducing the probability to hit these since they often take a long time to run
		switch r.Intn(5) {
		case 0:
			return s.genSingleIndexQuery(t, g, r, p)
		default:
			return s.genSinglePartitionQuery(t, g, r, p)
		}
	}
	return nil
}

func (s *Schema) genSinglePartitionQuery(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig) *Stmt {
	t.mu.RLock()
	defer t.mu.RUnlock()
	valuesWithToken := g.GetOld()
	if valuesWithToken == nil {
		return nil
	}
	var (
		mvCol    ColumnDef
		mvValues []interface{}
	)

	tableName := t.Name
	partitionKeys := t.PartitionKeys
	if len(t.MaterializedViews) > 0 && r.Int()%2 == 0 {
		view := r.Intn(len(t.MaterializedViews))
		tableName = t.MaterializedViews[view].Name
		partitionKeys = t.MaterializedViews[view].PartitionKeys
		mvCol = t.MaterializedViews[view].NonPrimaryKey
	}
	builder := qb.Select(s.Keyspace.Name + "." + tableName)
	typs := make([]Type, 0, 10)
	for _, pk := range partitionKeys {
		builder = builder.Where(qb.Eq(pk.Name))
		typs = append(typs, pk.Type)
	}
	if (ColumnDef{}) != mvCol {
		mvValues = appendValue(mvCol.Type, r, p, mvValues)
		mvValues = append(mvValues, valuesWithToken.Value...)
	}

	return &Stmt{
		ValuesWithToken: valuesWithToken,
		Query:           builder,
		Values:          mvValues,
		Types:           typs,
		QueryType:       SelectStatementType,
	}
}

func (s *Schema) genMultiplePartitionQuery(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig) *Stmt {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var (
		values []interface{}
		typs   []Type
	)
	tableName := t.Name
	partitionKeys := t.PartitionKeys
	if len(t.MaterializedViews) > 0 && r.Int()%2 == 0 {
		view := r.Intn(len(t.MaterializedViews))
		tableName = t.MaterializedViews[view].Name
		partitionKeys = t.MaterializedViews[view].PartitionKeys
	}
	numQueryPKs := r.Intn(len(partitionKeys))
	if numQueryPKs == 0 {
		numQueryPKs = 1
	}
	multiplier := int(math.Pow(float64(numQueryPKs), float64(len(partitionKeys))))
	if multiplier > 100 {
		numQueryPKs = 1
	}

	builder := qb.Select(s.Keyspace.Name + "." + tableName)
	for i, pk := range partitionKeys {
		builder = builder.Where(qb.InTuple(pk.Name, numQueryPKs))
		for j := 0; j < numQueryPKs; j++ {
			vs := g.GetOld()
			if vs == nil {
				return nil
			}
			numMVKeys := len(partitionKeys) - len(vs.Value)
			if i < numMVKeys {
				values = appendValue(pk.Type, r, p, values)
				typs = append(typs, pk.Type)
			} else {
				values = append(values, vs.Value[i-numMVKeys])
				typs = append(typs, pk.Type)
			}
		}
	}
	return &Stmt{
		Query:     builder,
		Values:    values,
		Types:     typs,
		QueryType: SelectStatementType,
	}
}

func (s *Schema) genClusteringRangeQuery(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig) *Stmt {
	t.mu.RLock()
	defer t.mu.RUnlock()
	vs := g.GetOld()
	if vs == nil {
		return nil
	}

	var (
		allTypes []Type
		mvCol    ColumnDef
		mvValues []interface{}
	)
	tableName := t.Name
	partitionKeys := t.PartitionKeys
	clusteringKeys := t.ClusteringKeys
	if len(t.MaterializedViews) > 0 && r.Int()%2 == 0 {
		view := r.Intn(len(t.MaterializedViews))
		tableName = t.MaterializedViews[view].Name
		partitionKeys = t.MaterializedViews[view].PartitionKeys
		clusteringKeys = t.MaterializedViews[view].ClusteringKeys
		mvCol = t.MaterializedViews[view].NonPrimaryKey
	}
	builder := qb.Select(s.Keyspace.Name + "." + tableName)
	values := vs.Value
	for _, pk := range partitionKeys {
		builder = builder.Where(qb.Eq(pk.Name))
		allTypes = append(allTypes, pk.Type)
	}
	if (ColumnDef{}) != mvCol {
		mvValues = appendValue(mvCol.Type, r, p, mvValues)
		values = append(mvValues, values...)
	}
	if len(clusteringKeys) > 0 {
		maxClusteringRels := r.Intn(len(clusteringKeys))
		for i := 0; i < maxClusteringRels; i++ {
			builder = builder.Where(qb.Eq(clusteringKeys[i].Name))
			values = appendValue(clusteringKeys[i].Type, r, p, values)
			allTypes = append(allTypes, clusteringKeys[i].Type)
		}
		builder = builder.Where(qb.Gt(clusteringKeys[maxClusteringRels].Name)).Where(qb.Lt(clusteringKeys[maxClusteringRels].Name))
		values = appendValue(t.ClusteringKeys[maxClusteringRels].Type, r, p, values)
		values = appendValue(t.ClusteringKeys[maxClusteringRels].Type, r, p, values)
		allTypes = append(allTypes, clusteringKeys[maxClusteringRels].Type, clusteringKeys[maxClusteringRels].Type)
	}
	return &Stmt{
		Query:     builder,
		Values:    values,
		Types:     allTypes,
		QueryType: SelectRangeStatementType,
	}
}

func (s *Schema) genMultiplePartitionClusteringRangeQuery(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig) *Stmt {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var (
		values []interface{}
		typs   []Type
	)
	tableName := t.Name
	partitionKeys := t.PartitionKeys
	clusteringKeys := t.ClusteringKeys
	if len(t.MaterializedViews) > 0 && r.Int()%2 == 0 {
		view := r.Intn(len(t.MaterializedViews))
		tableName = t.MaterializedViews[view].Name
		partitionKeys = t.MaterializedViews[view].PartitionKeys
		clusteringKeys = t.MaterializedViews[view].ClusteringKeys
	}
	numQueryPKs := r.Intn(len(partitionKeys))
	if numQueryPKs == 0 {
		numQueryPKs = 1
	}
	multiplier := int(math.Pow(float64(numQueryPKs), float64(len(partitionKeys))))
	if multiplier > 100 {
		numQueryPKs = 1
	}
	builder := qb.Select(s.Keyspace.Name + "." + tableName)
	for i, pk := range partitionKeys {
		builder = builder.Where(qb.InTuple(pk.Name, numQueryPKs))
		for j := 0; j < numQueryPKs; j++ {
			vs := g.GetOld()
			if vs == nil {
				return nil
			}
			numMVKeys := len(partitionKeys) - len(vs.Value)
			if i < numMVKeys {
				values = appendValue(pk.Type, r, p, values)
				typs = append(typs, pk.Type)
			} else {
				values = append(values, vs.Value[i-numMVKeys])
				typs = append(typs, pk.Type)
			}
		}
	}
	if len(clusteringKeys) > 0 {
		maxClusteringRels := r.Intn(len(clusteringKeys))
		for i := 0; i < maxClusteringRels; i++ {
			builder = builder.Where(qb.Eq(clusteringKeys[i].Name))
			values = appendValue(clusteringKeys[i].Type, r, p, values)
			typs = append(typs, clusteringKeys[i].Type)
		}
		builder = builder.Where(qb.Gt(clusteringKeys[maxClusteringRels].Name)).Where(qb.Lt(clusteringKeys[maxClusteringRels].Name))
		values = appendValue(clusteringKeys[maxClusteringRels].Type, r, p, values)
		values = appendValue(clusteringKeys[maxClusteringRels].Type, r, p, values)
		typs = append(typs, clusteringKeys[maxClusteringRels].Type, clusteringKeys[maxClusteringRels].Type)
	}
	return &Stmt{
		Query:     builder,
		Values:    values,
		Types:     typs,
		QueryType: SelectRangeStatementType,
	}
}

func (s *Schema) genSingleIndexQuery(t *Table, g *Generator, r *rand.Rand, p *PartitionRangeConfig) *Stmt {
	t.mu.RLock()
	defer t.mu.RUnlock()

	var (
		values []interface{}
		typs   []Type
	)

	if len(t.Indexes) == 0 {
		return nil
	}

	pkNum := r.Intn(len(t.Indexes))
	if pkNum == 0 {
		pkNum = 1
	}
	indexes := t.Indexes[:pkNum]
	builder := qb.Select(s.Keyspace.Name + "." + t.Name)
	builder.AllowFiltering()
	for _, idx := range indexes {
		builder = builder.Where(qb.Eq(idx.Column))
		values = appendValue(t.Columns[idx.ColumnIdx].Type, r, p, values)
		typs = append(typs, t.Columns[idx.ColumnIdx].Type)
	}

	return &Stmt{
		Query:     builder,
		Values:    values,
		Types:     typs,
		QueryType: SelectByIndexStatementType,
	}
}

type SchemaBuilder interface {
	Keyspace(Keyspace) SchemaBuilder
	Table(*Table) SchemaBuilder
	Build() *Schema
}

type schemaBuilder struct {
	keyspace Keyspace
	tables   []*Table
}

func (s *schemaBuilder) Keyspace(keyspace Keyspace) SchemaBuilder {
	s.keyspace = keyspace
	return s
}

func (s *schemaBuilder) Table(table *Table) SchemaBuilder {
	s.tables = append(s.tables, table)
	return s
}

func (s *schemaBuilder) Build() *Schema {
	return &Schema{Keyspace: s.keyspace, Tables: s.tables}
}

func NewSchemaBuilder() SchemaBuilder {
	return &schemaBuilder{}
}

func isCounterTable(t *Table) bool {
	if len(t.Columns) == 1 {
		switch t.Columns[0].Type.(type) {
		case *CounterType:
			return true
		}
	}
	return false
}
