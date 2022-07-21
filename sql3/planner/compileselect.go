// Copyright 2022 Molecula Corp. All rights reserved.

package planner

import (
	"context"

	pilosa "github.com/molecula/featurebase/v3"
	"github.com/molecula/featurebase/v3/sql3"
	"github.com/molecula/featurebase/v3/sql3/parser"
	"github.com/molecula/featurebase/v3/sql3/planner/types"
	"github.com/pkg/errors"
)

// compileSelectStatment compiles a parser.SelectStatment AST into a PlanOperator
func (p *ExecutionPlanner) compileSelectStatement(stmt *parser.SelectStatement, isSubquery bool) (types.PlanOperator, error) {
	query := NewPlanOpQuery(NewPlanOpNullTable(), p.sql)
	//p.pushPlannerScope(query)
	p.scopeStack.push(query)

	// handle select list
	projections := make([]types.PlanExpression, 0)
	for _, c := range stmt.Columns {
		planExpr, err := p.compileExpr(c.Expr)
		if err != nil {
			return nil, errors.Wrap(err, "planning select column expression")
		}
		if c.Alias != nil {
			planExpr = newAliasPlanExpression(c.Alias.Name, planExpr)
		}
		projections = append(projections, planExpr)
	}

	// group by clause.
	groupByExprs := make([]types.PlanExpression, 0)
	for _, expr := range stmt.GroupByExprs {
		switch expr := expr.(type) {
		case *parser.QualifiedRef:
			groupByExprs = append(groupByExprs, newQualifiedRefPlanExpression(expr.Table.Name, expr.Column.Name, expr.ColumnIndex, expr.DataType()))
		default:
			return nil, sql3.NewErrInternalf("unsupported expression type in GROUP BY clause: %T", expr)
		}
	}
	var err error

	if stmt.Having.IsValid() {
		query.AddWarning("HAVING is not yet supported")
	}

	// handle distinct
	if stmt.Distinct.IsValid() {
		query.AddWarning("DISTINCT not yet implemented")
	}

	// source expression last
	source, err := p.compileSelectSource(query, stmt.WhereExpr, stmt.Source)
	if err != nil {
		return nil, err
	}

	//do we have straight projection or a group by?
	var compiledOp types.PlanOperator
	if len(query.aggregates) > 0 {
		compiledOp = NewPlanOpProjection(projections, NewPlanOpGroupBy(query.aggregates, groupByExprs, source))
	} else {
		compiledOp = NewPlanOpProjection(projections, source)
	}

	// handle order by
	if len(stmt.OrderingTerms) > 0 {
		orderByFields := make([]*OrderByExpression, 0)
		for _, ot := range stmt.OrderingTerms {
			otExpr, err := p.compileExpr(ot.X)
			if err != nil {
				return nil, err
			}
			f := &OrderByExpression{
				Expr: otExpr,
			}
			f.Order = orderByAsc
			if ot.Desc.IsValid() {
				f.Order = orderByDesc
			}
			orderByFields = append(orderByFields, f)
		}
		compiledOp = NewPlanOpOrderBy(orderByFields, compiledOp)
	}

	//insert the top operator if it exists
	if stmt.Top.IsValid() {
		topExpr, err := p.compileExpr(stmt.TopExpr)
		if err != nil {
			return nil, err
		}
		compiledOp = NewPlanOpTop(topExpr, compiledOp)
	}

	//pop the scope
	//p.popPlannerScope()
	_ = p.scopeStack.pop()

	//if it is a subquery, don't wrap in a PlanOpQuery
	if isSubquery {
		return compiledOp, nil
	}
	children := []types.PlanOperator{
		compiledOp,
	}
	return query.WithChildren(children...)
}

func (p *ExecutionPlanner) compileSelectSource(scope *PlanOpQuery, whereExpr parser.Expr, source parser.Source) (types.PlanOperator, error) {
	if source == nil {
		return NewPlanOpNullTable(), nil
	}

	switch sourceExpr := source.(type) {
	case *parser.JoinClause:
		topOp, err := p.compileSelectSource(scope, whereExpr, sourceExpr.X)
		if err != nil {
			return nil, err
		}
		bottomOp, err := p.compileSelectSource(scope, whereExpr, sourceExpr.Y)
		if err != nil {
			return nil, err
		}
		scope.AddWarning("🦖 here there be dragons! JOINS are experimental.")
		if sourceExpr.Constraint == nil {
			scope.AddWarning("⚠️  cartesian products are never a good idea - are you missing a join constraint?")
		}
		return NewPlanOpNestedLoops(topOp, bottomOp), nil

	case *parser.QualifiedTableName:
		//get all the qualified refs that refer to this table
		extractColumns := []types.PlanExpression{}

		for _, r := range scope.referenceList {
			if sourceExpr.MatchesTablenameOrAlias(r.tableName) {
				extractColumns = append(extractColumns, r)
			}
		}

		// handle the where clause
		where, err := p.compileExpr(whereExpr)
		if err != nil {
			return nil, err
		}

		//get for the table name
		tableName := parser.IdentName(sourceExpr.Name)

		return NewPlanOpPQLTableScan(p, tableName, extractColumns, where), nil

	case *parser.ParenSource:
		return p.compileSelectSource(scope, whereExpr, sourceExpr.X)

	case *parser.SelectStatement:
		subQuery, err := p.compileSelectStatement(sourceExpr, true)
		if err != nil {
			return nil, err
		}
		return NewPlanOpSubquery(subQuery), nil

	default:
		return nil, sql3.NewErrInternalf("unexpected source type: %T", source)
	}
}

func (p *ExecutionPlanner) analyzeSource(source parser.Source) error {
	if source == nil {
		return nil
	}
	switch source := source.(type) {
	case *parser.JoinClause:
		err := p.analyzeSource(source.X)
		if err != nil {
			return err
		}
		err = p.analyzeSource(source.Y)
		if err != nil {
			return err
		}
		return nil

	case *parser.ParenSource:
		err := p.analyzeSource(source.X)
		if err != nil {
			return err
		}
		return nil

	case *parser.QualifiedTableName:
		//check table exists
		tableName := parser.IdentName(source.Name)
		table, err := p.schemaAPI.IndexInfo(context.Background(), tableName)
		if err != nil {
			if errors.Is(err, pilosa.ErrIndexNotFound) {
				return sql3.NewErrTableNotFound(source.Name.NamePos.Line, source.Name.NamePos.Column, tableName)
			}
			return err
		}

		// populate the output columns from the source
		for idx, fld := range table.Fields {
			soc := &parser.SourceOutputColumn{
				TableName:   tableName,
				ColumnName:  fld.Name,
				ColumnIndex: idx,
				Datatype:    fieldSQLDataType(fld),
			}
			source.OutputColumns = append(source.OutputColumns, soc)
		}

		return nil

	case *parser.SelectStatement:
		err := p.analyzeSelectStatement(source)
		if err != nil {
			return err
		}
		return nil

	default:
		return sql3.NewErrInternalf("unexpected source type: %T", source)
	}
}

func (p *ExecutionPlanner) analyzeSelectStatement(stmt *parser.SelectStatement) error {
	//analyze source first - needed for name resolution
	err := p.analyzeSource(stmt.Source)
	if err != nil {
		return err
	}

	if err := p.analyzeSelectStatementWildcards(stmt); err != nil {
		return err
	}

	for _, col := range stmt.Columns {
		expr, err := p.analyzeExpression(col.Expr, stmt)
		if err != nil {
			return err
		}
		if expr != nil {
			col.Expr = expr
		}
	}

	expr, err := p.analyzeExpression(stmt.TopExpr, stmt)
	if err != nil {
		return err
	}
	if expr != nil {
		if !(expr.IsLiteral() && typeIsInteger(expr.DataType())) {
			return sql3.NewErrIntegerLiteral(stmt.TopExpr.Pos().Line, stmt.TopExpr.Pos().Column)
		}
		stmt.TopExpr = expr
	}

	expr, err = p.analyzeExpression(stmt.WhereExpr, stmt)
	if err != nil {
		return err
	}
	stmt.WhereExpr = expr

	for i, g := range stmt.GroupByExprs {
		expr, err = p.analyzeExpression(g, stmt)
		if err != nil {
			return err
		}
		if expr != nil {
			stmt.GroupByExprs[i] = expr
		}
	}

	expr, err = p.analyzeExpression(stmt.HavingExpr, stmt)
	if err != nil {
		return err
	}
	if expr != nil {
		stmt.HavingExpr = expr
	}

	for _, term := range stmt.OrderingTerms {
		expr, err = p.analyzeExpression(term.X, stmt)
		if err != nil {
			return err
		}
		if expr != nil {
			term.X = expr
		}
	}

	return nil
}

func (p *ExecutionPlanner) analyzeSelectStatementWildcards(stmt *parser.SelectStatement) error {
	if !stmt.HasWildcard() {
		return nil
	}

	// replace wildcards with column references
	newColumns := make([]*parser.ResultColumn, 0, len(stmt.Columns))
	for _, col := range stmt.Columns {

		//handle the case of unqualified *
		if col.Star.IsValid() {

			cols, err := p.columnsFromSource(stmt.Source)
			if err != nil {
				return err
			}
			newColumns = append(newColumns, cols...)

		} else {
			//handle the case of a qualified ref with a *
			if ref, ok := col.Expr.(*parser.QualifiedRef); ok && ref.Star.IsValid() {
				refName := parser.IdentName(ref.Table)
				src := stmt.Source.SourceFromAlias(refName)
				if src == nil {
					return sql3.NewErrTableNotFound(ref.Table.NamePos.Line, ref.Table.NamePos.Column, refName)
				}

				cols, err := p.columnsFromSource(src)
				if err != nil {
					return err
				}
				newColumns = append(newColumns, cols...)

			} else {
				//add the column as is...
				newColumns = append(newColumns, col)
			}
		}
	}
	stmt.Columns = newColumns

	return nil
}

func (p *ExecutionPlanner) columnsFromSource(source parser.Source) ([]*parser.ResultColumn, error) {
	result := []*parser.ResultColumn{}

	switch src := source.(type) {
	case *parser.JoinClause:
		return nil, sql3.NewErrInternal("joins are not currently supported")
	case *parser.ParenSource:
		for _, oc := range src.PossibleOutputColumns() {
			result = append(result, &parser.ResultColumn{
				Expr: &parser.QualifiedRef{
					Table:       &parser.Ident{Name: oc.TableName},
					Column:      &parser.Ident{Name: oc.ColumnName},
					ColumnIndex: oc.ColumnIndex,
				},
			})
		}
		return result, nil

	case *parser.QualifiedTableName:
		for _, oc := range src.PossibleOutputColumns() {
			result = append(result, &parser.ResultColumn{
				Expr: &parser.QualifiedRef{
					Table:       &parser.Ident{Name: oc.TableName},
					Column:      &parser.Ident{Name: oc.ColumnName},
					ColumnIndex: oc.ColumnIndex,
				},
			})
		}
		return result, nil

	case *parser.SelectStatement:
		return nil, sql3.NewErrInternal("sub-selects are not currently supported")
	default:
		return nil, sql3.NewErrInternalf("unexpected source type: %T", source)
	}
}