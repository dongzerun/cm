// Copyright 2014, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package planbuilder

import (
	"fmt"

	"github.com/ngaut/arena"
	"github.com/wandoulabs/cm/sqlparser"
	"github.com/wandoulabs/cm/vt/schema"
)

func GenerateFullQuery(statement sqlparser.Statement, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	statement.Format(buf)
	return buf.ParsedQuery()
}

func GenerateFieldQuery(statement sqlparser.Statement, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(FormatImpossible, alloc)
	buf.Myprintf("%v", statement)
	if buf.HasBindVars() {
		return nil
	}
	return buf.ParsedQuery()
}

// FormatImpossible is a callback function used by TrackedBuffer
// to generate a modified version of the query where all selects
// have impossible where clauses. It overrides a few node types
// and passes the rest down to the default FormatNode.
func FormatImpossible(buf *sqlparser.TrackedBuffer, node sqlparser.SQLNode) {
	switch node := node.(type) {
	case *sqlparser.Select:
		buf.Myprintf("select %v from %v where 1 != 1", node.SelectExprs, node.From)
	case *sqlparser.JoinTableExpr:
		if node.Join == sqlparser.AST_LEFT_JOIN || node.Join == sqlparser.AST_RIGHT_JOIN {
			// ON clause is requried
			buf.Myprintf("%v %s %v on 1 != 1", node.LeftExpr, node.Join, node.RightExpr)
		} else {
			buf.Myprintf("%v %s %v", node.LeftExpr, node.Join, node.RightExpr)
		}
	default:
		node.Format(buf)
	}
}

func GenerateSelectLimitQuery(selStmt sqlparser.SelectStatement, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	sel, ok := selStmt.(*sqlparser.Select)
	if ok {
		limit := sel.Limit
		if limit == nil {
			sel.Limit = execLimit
			defer func() {
				sel.Limit = nil
			}()
		}
	}
	buf.Myprintf("%v", selStmt)
	return buf.ParsedQuery()
}

func GenerateSelectOuterQuery(sel *sqlparser.Select, tableInfo *schema.Table, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	fmt.Fprintf(buf, "select ")
	writeColumnList(buf, tableInfo.Columns)
	buf.Myprintf(" from %v where %a", sel.From, ":#pk")
	return buf.ParsedQuery()
}

func GenerateReplaceOuterQuery(ins *sqlparser.Replace, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	buf.Myprintf("replace %vinto %v%v values %a%v",
		ins.Comments,
		ins.Table,
		ins.Columns,
		":#values",
		ins.OnDup,
	)
	return buf.ParsedQuery()
}

func GenerateInsertOuterQuery(ins *sqlparser.Insert, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	buf.Myprintf("insert %vinto %v%v values %a%v",
		ins.Comments,
		ins.Table,
		ins.Columns,
		":#values",
		ins.OnDup,
	)
	return buf.ParsedQuery()
}

func GenerateUpdateOuterQuery(upd *sqlparser.Update, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	buf.Myprintf("update %v%v set %v where %a", upd.Comments, upd.Table, upd.Exprs, ":#pk")
	return buf.ParsedQuery()
}

func GenerateDeleteOuterQuery(del *sqlparser.Delete, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	buf.Myprintf("delete %vfrom %v where %a", del.Comments, del.Table, ":#pk")
	return buf.ParsedQuery()
}

func GenerateSelectSubquery(sel *sqlparser.Select, tableInfo *schema.Table, index string, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	hint := &sqlparser.IndexHints{Type: sqlparser.AST_USE, Indexes: [][]byte{[]byte(index)}}
	table_expr := sel.From[0].(*sqlparser.AliasedTableExpr)
	savedHint := table_expr.Hints
	table_expr.Hints = hint
	defer func() {
		table_expr.Hints = savedHint
	}()
	return GenerateSubquery(
		tableInfo.Indexes[0].Columns,
		table_expr,
		sel.Where,
		sel.OrderBy,
		sel.Limit,
		false,
		alloc,
	)
}

func GenerateUpdateSubquery(upd *sqlparser.Update, tableInfo *schema.Table, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	return GenerateSubquery(
		tableInfo.Indexes[0].Columns,
		&sqlparser.AliasedTableExpr{Expr: upd.Table},
		upd.Where,
		upd.OrderBy,
		upd.Limit,
		true,
		alloc,
	)
}

func GenerateDeleteSubquery(del *sqlparser.Delete, tableInfo *schema.Table, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	return GenerateSubquery(
		tableInfo.Indexes[0].Columns,
		&sqlparser.AliasedTableExpr{Expr: del.Table},
		del.Where,
		del.OrderBy,
		del.Limit,
		true,
		alloc,
	)
}

func GenerateSubquery(columns []string, table *sqlparser.AliasedTableExpr, where *sqlparser.Where, order sqlparser.OrderBy, limit *sqlparser.Limit, for_update bool, alloc arena.ArenaAllocator) *sqlparser.ParsedQuery {
	buf := sqlparser.NewTrackedBuffer(nil, alloc)
	if limit == nil {
		limit = execLimit
	}
	fmt.Fprintf(buf, "select ")
	i := 0
	for i = 0; i < len(columns)-1; i++ {
		fmt.Fprintf(buf, "%s, ", columns[i])
	}
	fmt.Fprintf(buf, "%s", columns[i])
	buf.Myprintf(" from %v%v%v%v", table, where, order, limit)
	if for_update {
		buf.Myprintf(sqlparser.AST_FOR_UPDATE)
	}
	return buf.ParsedQuery()
}

func writeColumnList(buf *sqlparser.TrackedBuffer, columns []schema.TableColumn) {
	i := 0
	for i = 0; i < len(columns)-1; i++ {
		fmt.Fprintf(buf, "%s, ", columns[i].Name)
	}
	fmt.Fprintf(buf, "%s", columns[i].Name)
}
