// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package syncer

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb-enterprise-tools/pkg/filter"
	"github.com/pingcap/tidb/ast"
	"github.com/pingcap/tidb/mysql"
	"github.com/pingcap/tidb/parser"
)

var (
	// ErrDMLStatementFound defines an error which means we found unexpected dml statement found in query event.
	ErrDMLStatementFound = errors.New("unexpected dml statement found in query event")
	// IncompatibleDDLFormat is for incompatible ddl
	IncompatibleDDLFormat = `encountered incompatible DDL in TiDB: %s
	please confirm your DDL statement is correct and needed.
	for TiDB compatible DDL, please see the docs:
	  English version: https://github.com/pingcap/docs/blob/master/sql/ddl.md
	  Chinese version: https://github.com/pingcap/docs-cn/blob/master/sql/ddl.md
	if the DDL is not needed, you can use dm-ctl to skip it, otherwise u also can use dm-ctl to replace it.
	 `
)

// trimCtrlChars returns a slice of the string s with all leading
// and trailing control characters removed.
func trimCtrlChars(s string) string {
	f := func(r rune) bool {
		// All entries in the ASCII table below code 32 (technically the C0 control code set) are of this kind,
		// including CR and LF used to separate lines of text. The code 127 (DEL) is also a control character.
		// Reference: https://en.wikipedia.org/wiki/Control_character
		return r < 32 || r == 127
	}

	return strings.TrimFunc(s, f)
}

// resolveDDLSQL resolve to one ddl sql
// example: drop table test.a,test2.b -> drop table test.a; drop table test2.b;
func resolveDDLSQL(sql string, p *parser.Parser) (sqls []string, err error) {
	sql = trimCtrlChars(sql)
	// We use Parse not ParseOneStmt here, because sometimes we got a commented out ddl which can't be parsed
	// by ParseOneStmt(it's a limitation of tidb parser.)
	stmts, err := p.Parse(sql, "", "")
	if err != nil {
		// log error rather than fatal, so other defer can be executed
		log.Errorf(IncompatibleDDLFormat, sql)
		return []string{sql}, errors.Annotatef(err, IncompatibleDDLFormat, sql)
	}

	if len(stmts) == 0 {
		return nil, nil
	}

	stmt := stmts[0]
	switch stmt.(type) {
	case ast.DDLNode:
		// do nothing
	case ast.DMLNode:
		return nil, errors.Annotatef(ErrDMLStatementFound, "query %s", sql)
	default:
		// BEGIN statement is included here.
		// let sqls be empty
		return sqls, nil
	}

	switch v := stmt.(type) {
	case *ast.DropTableStmt:
		var ex string
		if v.IfExists {
			ex = "IF EXISTS "
		}
		for _, t := range v.Tables {
			var db string
			if t.Schema.O != "" {
				db = fmt.Sprintf("`%s`.", t.Schema.O)
			}
			s := fmt.Sprintf("DROP TABLE %s%s`%s`", ex, db, t.Name.O)
			sqls = append(sqls, s)
		}
	case *ast.AlterTableStmt:
		tempSpecs := v.Specs
		newTable := &ast.TableName{}
		log.Warnf("will split alter table statement: %v", sql)
		for i := range tempSpecs {
			v.Specs = tempSpecs[i : i+1]
			splitted := alterTableStmtToSQL(v, newTable)
			log.Warnf("splitted alter table statement: %v", splitted)
			sqls = append(sqls, splitted...)
		}
	case *ast.RenameTableStmt:
		for _, t2t := range v.TableToTables {
			sqlNew := fmt.Sprintf("RENAME TABLE %s TO %s", tableNameToSQL(t2t.OldTable), tableNameToSQL(t2t.NewTable))
			sqls = append(sqls, sqlNew)
		}

	default:
		sqls = append(sqls, sql)
	}

	return sqls, nil
}

// todo: fix the ugly code, use ast to rename table
func genDDLSQL(sql string, stmt ast.StmtNode, originTableNames []*filter.Table, targetTableNames []*filter.Table) (string, error) {
	addUseDatabase := func(sql string, dbName string) string {
		return fmt.Sprintf("USE `%s`; %s;", dbName, sql)
	}

	if notNeedRoute(originTableNames, targetTableNames) {
		_, isCreateDatabase := stmt.(*ast.CreateDatabaseStmt)
		if isCreateDatabase {
			return fmt.Sprintf("%s;", sql), nil
		}

		return addUseDatabase(sql, originTableNames[0].Schema), nil
	}

	switch stmt.(type) {
	case *ast.CreateDatabaseStmt:
		sqlPrefix := createDatabaseRegex.FindString(sql)
		index := findLastWord(sqlPrefix)
		return createDatabaseRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`", sqlPrefix[:index], targetTableNames[0].Schema)), nil

	case *ast.DropDatabaseStmt:
		sqlPrefix := dropDatabaseRegex.FindString(sql)
		index := findLastWord(sqlPrefix)
		return dropDatabaseRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`", sqlPrefix[:index], targetTableNames[0].Schema)), nil

	case *ast.CreateTableStmt:
		var (
			sqlPrefix string
			index     int
		)
		// replace `like schema.table` section
		if len(originTableNames) == 2 {
			sqlPrefix = createTableLikeRegex.FindString(sql)
			index = findLastWord(sqlPrefix)
			endChars := ""
			if sqlPrefix[len(sqlPrefix)-1] == ')' {
				endChars = ")"
			}
			sql = createTableLikeRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`.`%s`%s", sqlPrefix[:index], targetTableNames[1].Schema, targetTableNames[1].Name, endChars))
		}
		// replce `create table schame.table` section
		sqlPrefix = createTableRegex.FindString(sql)
		index = findLastWord(sqlPrefix)
		endChars := findTableDefineIndex(sqlPrefix[index:])
		sql = createTableRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`.`%s`%s", sqlPrefix[:index], targetTableNames[0].Schema, targetTableNames[0].Name, endChars))

	case *ast.DropTableStmt:
		sqlPrefix := dropTableRegex.FindString(sql)
		index := findLastWord(sqlPrefix)
		sql = dropTableRegex.ReplaceAllString(sql, fmt.Sprintf("%s`%s`.`%s`", sqlPrefix[:index], targetTableNames[0].Schema, targetTableNames[0].Name))

	case *ast.TruncateTableStmt:
		sql = fmt.Sprintf("TRUNCATE TABLE `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name)

	case *ast.AlterTableStmt:
		// RENAME [TO|AS] new_tbl_name
		if len(originTableNames) == 2 {
			index := findLastWord(sql)
			sql = fmt.Sprintf("%s`%s`.`%s`", sql[:index], targetTableNames[1].Schema, targetTableNames[1].Name)
		}
		sql = alterTableRegex.ReplaceAllString(sql, fmt.Sprintf("ALTER TABLE `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name))

	case *ast.RenameTableStmt:
		return fmt.Sprintf("RENAME TABLE `%s`.`%s` TO `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name,
			targetTableNames[1].Schema, targetTableNames[1].Name), nil

	case *ast.CreateIndexStmt:
		sql = createIndexDDLRegex.ReplaceAllString(sql, fmt.Sprintf("ON `%s`.`%s` (", targetTableNames[0].Schema, targetTableNames[0].Name))

	case *ast.DropIndexStmt:
		sql = dropIndexDDLRegex.ReplaceAllString(sql, fmt.Sprintf("ON `%s`.`%s`", targetTableNames[0].Schema, targetTableNames[0].Name))

	default:
		return "", errors.Errorf("unkown type ddl %s", sql)
	}

	return addUseDatabase(sql, targetTableNames[0].Schema), nil
}

func notNeedRoute(originTableNames []*filter.Table, targetTableNames []*filter.Table) bool {
	for index, originTableName := range originTableNames {
		targetTableName := targetTableNames[index]
		if originTableName.Schema != targetTableName.Schema {
			return false
		}
		if originTableName.Name != targetTableName.Name {
			return false
		}
	}
	return true
}

func findLastWord(literal string) int {
	index := len(literal) - 1
	for index >= 0 && literal[index] == ' ' {
		index--
	}

	for index >= 0 {
		if literal[index-1] == ' ' {
			return index
		}
		index--
	}
	return index
}

func findTableDefineIndex(literal string) string {
	for i := range literal {
		if literal[i] == '(' {
			return literal[i:]
		}
	}
	return ""
}

func genTableName(schema string, table string) *filter.Table {
	return &filter.Table{Schema: schema, Name: table}

}

// the result contains [tableName] excepted create table like and rename table
// for `create table like` DDL, result contains [sourceTableName, sourceRefTableName]
// for rename table ddl, result contains [targetOldTableName, sourceNewTableName]
func fetchDDLTableNames(schema string, stmt ast.StmtNode) ([]*filter.Table, error) {
	var res []*filter.Table
	switch v := stmt.(type) {
	case *ast.CreateDatabaseStmt:
		res = append(res, genTableName(v.Name, ""))
	case *ast.DropDatabaseStmt:
		res = append(res, genTableName(v.Name, ""))
	case *ast.CreateTableStmt:
		res = append(res, genTableName(v.Table.Schema.O, v.Table.Name.O))
		if v.ReferTable != nil {
			res = append(res, genTableName(v.ReferTable.Schema.O, v.ReferTable.Name.O))
		}
	case *ast.DropTableStmt:
		if len(v.Tables) != 1 {
			return res, errors.Errorf("drop table with multiple tables, may resovle ddl sql failed")
		}
		res = append(res, genTableName(v.Tables[0].Schema.O, v.Tables[0].Name.O))
	case *ast.TruncateTableStmt:
		res = append(res, genTableName(v.Table.Schema.O, v.Table.Name.O))
	case *ast.AlterTableStmt:
		res = append(res, genTableName(v.Table.Schema.O, v.Table.Name.O))
		if v.Specs[0].NewTable != nil {
			res = append(res, genTableName(v.Specs[0].NewTable.Schema.O, v.Specs[0].NewTable.Name.O))
		}
	case *ast.RenameTableStmt:
		res = append(res, genTableName(v.OldTable.Schema.O, v.OldTable.Name.O))
		res = append(res, genTableName(v.NewTable.Schema.O, v.NewTable.Name.O))
	case *ast.CreateIndexStmt:
		res = append(res, genTableName(v.Table.Schema.O, v.Table.Name.O))
	case *ast.DropIndexStmt:
		res = append(res, genTableName(v.Table.Schema.O, v.Table.Name.O))
	default:
		return res, errors.Errorf("unkown type ddl %s", stmt)
	}

	for i := range res {
		if res[i].Schema == "" {
			res[i].Schema = schema
		}
	}

	return res, nil
}

func (s *Syncer) handleDDL(p *parser.Parser, schema, sql string) (string, [][]*filter.Table, ast.StmtNode, error) {
	stmt, err := p.ParseOneStmt(sql, "", "")
	if err != nil {
		return "", nil, nil, errors.Annotatef(err, "ddl %s", sql)
	}

	tableNames, err := fetchDDLTableNames(schema, stmt)
	if err != nil {
		return "", nil, nil, errors.Trace(err)
	}

	ignore, err := s.skipQuery(tableNames, stmt, sql)
	if err != nil {
		return "", nil, nil, errors.Trace(err)
	}
	if ignore {
		return "", nil, stmt, nil
	}

	var targetTableNames []*filter.Table
	for i := range tableNames {
		schema, table := s.renameShardingSchema(tableNames[i].Schema, tableNames[i].Name)
		tableName := &filter.Table{
			Schema: schema,
			Name:   table,
		}
		targetTableNames = append(targetTableNames, tableName)
	}

	ddl, err := genDDLSQL(sql, stmt, tableNames, targetTableNames)
	return ddl, [][]*filter.Table{tableNames, targetTableNames}, stmt, errors.Trace(err)
}

func getParser(db *sql.DB) (*parser.Parser, error) {
	parser := parser.New()
	ok, err := hasAnsiQuotesMode(db)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if ok {
		parser.SetSQLMode(mysql.ModeANSIQuotes)
	}
	return parser, nil
}

// fetchDDLSchema fetches schema name from StmtNode
func fetchDDLSchema(stmt ast.StmtNode) string {
	switch v := stmt.(type) {
	case *ast.CreateDatabaseStmt:
		return v.Name
	case *ast.DropDatabaseStmt:
		return v.Name
	case *ast.CreateTableStmt:
		return v.Table.Schema.O
	case *ast.DropTableStmt:
		return v.Tables[0].Schema.O
	case *ast.TruncateTableStmt:
		return v.Table.Schema.O
	case *ast.AlterTableStmt:
		return v.Table.Schema.O
	case *ast.RenameTableStmt:
		return v.OldTable.Schema.O
	case *ast.CreateIndexStmt:
		return v.Table.Schema.O
	case *ast.DropIndexStmt:
		return v.Table.Schema.O
	default:
		return ""
	}
}

func (s *Syncer) dropSchemaInSharding(sourceSchema string) error {
	sources := make(map[string][][]string)
	sgs := s.sgk.Groups()
	for name, sg := range sgs {
		tables := sg.Tables()
		for _, table := range tables {
			if table[0] != sourceSchema {
				continue
			}
			sources[name] = append(sources[name], table)
		}
	}
	// delete from sharding group firstly
	for name, tables := range sources {
		targetSchema, targetTable := UnpackTableID(name)
		sourceIDs := make([]string, 0, len(tables))
		for _, table := range tables {
			sourceID, _ := GenTableID(table[0], table[1])
			sourceIDs = append(sourceIDs, sourceID)
		}
		err := s.sgk.LeaveGroup(targetSchema, targetTable, sourceIDs)
		if err != nil {
			return errors.Trace(err)
		}
	}
	// delete from checkpoint
	for _, tables := range sources {
		for _, table := range tables {
			// refine clear them later if failed
			// now it doesn't have problems
			if err1 := s.checkpoint.DeleteTablePoint(table[0], table[1]); err1 != nil {
				log.Errorf("[syncer] fail to delete checkpoint %s.%s", table[0], table[1])
			}
		}
	}
	return nil
}

type shardingDDLInfo struct {
	name       string
	tableNames [][]*filter.Table
	stmt       ast.StmtNode
}
