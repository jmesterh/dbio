package database

import (
	"io/ioutil"
	"strings"

	"github.com/flarco/dbio/iop"
	"github.com/flarco/g"
	"github.com/samber/lo"
	"github.com/spf13/cast"
	"gopkg.in/yaml.v3"
)

/*
we have tables and where have no info on primary keys.

1. determine all PKs
- get columns
- get a sample, and iterate on each row. do
select
	count(1) tot_cnt,
	count({field}) {field}_cnt,
	count({field}) {field}_distct_cnt,
	min,
	max,
	min_len,
	max_len
from (
	select * from {schema}.{table}
	limit {limit}
) t

on each row, determine the unique ones and cross-reference on potential join matches and types
*/

type DataAnalyzerOptions struct {
	DbName      string
	SchemaNames []string
}

type DataAnalyzer struct {
	Conn        Connection
	Schemata    Schemata
	ColumnMap   map[string]iop.Column
	RelationMap map[string]map[string]map[string]Relation // table > column A > column B > relation
	Options     DataAnalyzerOptions
}

type Relation string

const RelationOneToOne = "one_to_one"
const RelationOneToMany = "one_to_many"
const RelationManyToOne = "many_to_one"
const RelationManyToMany = "many_to_many"

func NewDataAnalyzer(conn Connection, opts DataAnalyzerOptions) (da *DataAnalyzer, err error) {
	if len(opts.SchemaNames) == 0 {
		err = g.Error("must provide SchemaNames")
		return
	}

	err = conn.Connect()
	if err != nil {
		err = g.Error(err, "could not connect to database")
		return
	}

	da = &DataAnalyzer{
		Conn:        conn,
		Options:     opts,
		ColumnMap:   map[string]iop.Column{},
		RelationMap: map[string]map[string]map[string]Relation{},
		Schemata:    Schemata{Databases: map[string]Database{}},
	}

	return
}

func (da *DataAnalyzer) GetSchemata(force bool) (err error) {
	if !(force || len(da.Schemata.Databases) == 0) {
		return nil
	}

	for _, schema := range da.Options.SchemaNames {
		g.Info("getting schemata for %s", schema)
		schemata, err := da.Conn.GetSchemata(schema, "")
		if err != nil {
			return g.Error(err, "could not get schemata")
		}

		// merge into da.Schemata
		for dbKey, db := range schemata.Databases {
			if _, ok := da.Schemata.Databases[dbKey]; ok {
				for schemaKey, schema := range db.Schemas {
					da.Schemata.Databases[dbKey].Schemas[schemaKey] = schema
				}
			} else {
				da.Schemata.Databases[dbKey] = db
			}
		}
	}

	return
}

var sqlAnalyzeColumns = `
	select {cols_sql}
	from ( select * from {schema}.{table} limit {limit} ) t
`

type StatFieldSQL struct {
	Name        string
	TemplateSQL string
}

func (da *DataAnalyzer) AnalyzeColumns(sampleSize int) (err error) {
	err = da.GetSchemata(false)
	if err != nil {
		err = g.Error(err, "could not get schemata")
		return
	}

	fieldAsString := da.Conn.Template().Function["cast_to_string"]
	var statsFields = []StatFieldSQL{
		{"total_cnt", `count(1)`},
		{"null_cnt", `count(1) - count({field})`},
		{"uniq_cnt", `count(distinct {field})`},
		{"min_len", g.F(`min(length(%s))`, fieldAsString)},
		{"max_len", g.F(`max(length(%s))`, fieldAsString)},
		// {"value_minimun", "min({field}::text)"}, // pulls customer data
		// {"value_maximum", "max({field}::text)"}, // pulls customer data
	}

	for _, table := range da.Schemata.Tables() {
		g.Info("analyzing table %s", table.FullName())

		tableColMap := table.ColumnsMap()
		// g.PP(tableColMap)

		// need order to retrieve values
		colsAll := lo.Filter(lo.Values(tableColMap), func(c iop.Column, i int) bool {
			// t := strings.ToLower(c.DbType)
			isText := c.IsString() && c.Type != iop.JsonType
			// isNumber := strings.Contains(t, "int") || strings.Contains(t, "double")
			return isText
			// TODO: should be string or number?
			// skip date column for now
			return !(strings.Contains(c.Name, "time") || strings.Contains(c.Name, "date"))
		})

		g.Debug("getting stats for %d columns", len(colsAll))
		// chunk to not submit too many
		for _, cols := range lo.Chunk(colsAll, 100) {

			colsSQL := []string{}
			for _, col := range cols {
				for _, sf := range statsFields {
					colSQL := g.R(
						g.F("%s as {alias}_%s", sf.TemplateSQL, sf.Name),
						"field", da.Conn.Quote(col.Name),
						"alias", strings.ReplaceAll(col.Name, "-", "_"),
					)
					colsSQL = append(colsSQL, colSQL)
				}
			}

			sql := g.R(
				sqlAnalyzeColumns,
				"cols_sql", strings.Join(colsSQL, ", "),
				"schema", da.Conn.Quote(table.Schema),
				"table", da.Conn.Quote(table.Name),
				"limit", cast.ToString(sampleSize),
			)
			data, err := da.Conn.Query(sql)
			if err != nil {
				return g.Error(err, "could not get analysis sql for %s", table.FullName())
			} else if len(data.Rows) == 0 {
				return g.Error("got zero rows for analysis sql for %s", table.FullName())
			}

			// retrieve values, since in order
			row := data.Rows[0]
			i := 0
			for _, col := range cols {
				m := g.M()
				for _, sf := range statsFields {
					m[sf.Name] = row[i]
					i++
				}
				// unmarshal
				err = g.Unmarshal(g.Marshal(m), &col.Stats)
				if err != nil {
					return g.Error(err, "could not get unmarshal sql stats for %s:\n%s", table.FullName(), g.Marshal(m))
				}

				// store in master map
				da.ColumnMap[col.Key()] = col
				if col.IsUnique() {
					g.Info("    %s is unique [%d rows]", col.Key(), col.Stats.TotalCnt)
				}
			}
		}
	}

	return
}

func (da *DataAnalyzer) ProcessRelations() (err error) {

	// same length text fields, uuid
	lenColMap := map[int]iop.Columns{}
	for _, col := range da.ColumnMap {
		if col.IsString() && col.Type != iop.JsonType &&
			col.Stats.MaxLen-col.Stats.MinLen <= 2 && // min/max len are about the same
			col.Stats.MinLen > 0 {
			if arr, ok := lenColMap[col.Stats.MinLen]; ok {
				lenColMap[col.Stats.MinLen] = append(arr, col)
			} else {
				lenColMap[col.Stats.MinLen] = iop.Columns{col}
			}
		}
	}

	// iterate over non-unique ones, and grab a value, and try to join to unique ones
	uniqueCols := iop.Columns{}
	nonUniqueCols := iop.Columns{}
	for lenI, cols := range lenColMap {
		g.Info("%d | %d cols", lenI, len(cols))
		if lenI > 4 && len(cols) > 1 {
			// do process
		} else if !g.In(lenI, 36, 18, 19, 28, 27) {
			// only UUID (36), sfdc id (18), stripe (18, 19, 28, 27) for now
			continue
		}
		for _, col := range cols {
			if col.Stats.TotalCnt <= 1 {
				continue // skip single row tables for now
			} else if col.IsUnique() {
				uniqueCols = append(uniqueCols, col)
			} else {
				nonUniqueCols = append(nonUniqueCols, col)
			}
		}
	}

	err = da.GetOneToMany(uniqueCols, nonUniqueCols)
	if err != nil {
		return g.Error(err, "could not run GetOneToMany")
	}

	err = da.GetOneToOne(uniqueCols)
	if err != nil {
		return g.Error(err, "could not run GetOneToMany")
	}

	err = da.GetManyToMany(nonUniqueCols)
	if err != nil {
		return g.Error(err, "could not run GetOneToMany")
	}

	// save yaml
	out, err := yaml.Marshal(da.RelationMap)
	if err != nil {
		return g.Error(err, "could not marshal to yaml")
	}

	err = ioutil.WriteFile("relations.yaml", out, 0755)
	if err != nil {
		return g.Error(err, "could not write to yaml")
	}

	return
}

func (da *DataAnalyzer) GetOneToMany(uniqueCols, nonUniqueCols iop.Columns) (err error) {
	if len(uniqueCols) == 0 || len(nonUniqueCols) == 0 {
		return g.Error("len(uniqueCols) == %d || len(nonUniqueCols) == %d", len(uniqueCols), len(nonUniqueCols))
	}
	// build all_non_unique_values
	stringType := da.Conn.Template().Function["string_type"]
	nonUniqueExpressions := lo.Map(nonUniqueCols, func(col iop.Column, i int) string {
		template := `select * from (select '{col_key}' as non_unique_column_key, cast({field} as {string_type}) as val from {schema}.{table} where {field} is not null limit 1) t`
		return g.R(
			template,
			"col_key", col.Key(),
			"field", da.Conn.Quote(col.Name),
			"string_type", stringType,
			"schema", da.Conn.Quote(col.Schema),
			"table", da.Conn.Quote(col.Table),
		)
	})
	nonUniqueSQL := strings.Join(nonUniqueExpressions, " union all\n    ")

	// get 1-N and N-1
	matchingSQLs := lo.Map(uniqueCols, func(col iop.Column, i int) string {
		template := `select nuv.non_unique_column_key,	'{col_key}' as unique_column_key	from all_non_unique_values nuv
				inner join {schema}.{table} t on cast(t.{field} as {string_type}) = nuv.val`
		return g.R(
			template,
			"col_key", col.Key(),
			"field", da.Conn.Quote(col.Name),
			"string_type", stringType,
			"schema", da.Conn.Quote(col.Schema),
			"table", da.Conn.Quote(col.Table),
		)
	})
	matchingSQL := strings.Join(matchingSQLs, "    union all\n    ")

	// run
	sql := g.R(`with all_non_unique_values as (
				{non_unique_sql}
			)
			, matching as (
				{matching_sql}
			)
			select unique_column_key, non_unique_column_key
			from matching
			order by unique_column_key
			`,
		"non_unique_sql", nonUniqueSQL,
		"matching_sql", matchingSQL,
	)
	data, err := da.Conn.Query(sql)
	if err != nil {
		return g.Error(err, "could not get matching columns")
	}

	for _, rec := range data.Records() {
		uniqueColumnKey := cast.ToString(rec["unique_column_key"])
		nonUniqueColumnKey := cast.ToString(rec["non_unique_column_key"])

		uniqueColumn := da.ColumnMap[uniqueColumnKey]
		nonUniqueColumn := da.ColumnMap[nonUniqueColumnKey]

		uniqueColumnTable := g.F("%s.%s", uniqueColumn.Schema, uniqueColumn.Table)
		nonUniqueColumnTable := g.F("%s.%s", nonUniqueColumn.Schema, nonUniqueColumn.Table)

		// one to many
		if mt, ok := da.RelationMap[uniqueColumnTable]; ok {
			if m, ok := mt[uniqueColumnKey]; ok {
				m[nonUniqueColumnKey] = RelationOneToMany
			} else {
				mt[uniqueColumnKey] = map[string]Relation{nonUniqueColumnKey: RelationOneToMany}
			}
		} else {
			da.RelationMap[uniqueColumnTable] = map[string]map[string]Relation{
				uniqueColumnKey: {nonUniqueColumnKey: RelationOneToMany},
			}
		}

		// many to one
		if mt, ok := da.RelationMap[nonUniqueColumnTable]; ok {
			if m, ok := mt[nonUniqueColumnKey]; ok {
				m[uniqueColumnKey] = RelationManyToOne
			} else {
				mt[nonUniqueColumnKey] = map[string]Relation{uniqueColumnKey: RelationManyToOne}
			}
		} else {
			da.RelationMap[nonUniqueColumnTable] = map[string]map[string]Relation{
				nonUniqueColumnKey: {uniqueColumnKey: RelationManyToOne},
			}
		}
	}
	return
}

func (da *DataAnalyzer) GetOneToOne(uniqueCols iop.Columns) (err error) {
	stringType := da.Conn.Template().Function["string_type"]
	uniqueExpressions := lo.Map(uniqueCols, func(col iop.Column, i int) string {
		template := `select * from (select '{col_key}' as unique_column_key_1, cast({field} as {string_type}) as val from {schema}.{table} where {field} is not null limit 1) t`
		return g.R(
			template,
			"col_key", col.Key(),
			"field", da.Conn.Quote(col.Name),
			"string_type", stringType,
			"schema", da.Conn.Quote(col.Schema),
			"table", da.Conn.Quote(col.Table),
		)
	})
	nonUniqueSQL := strings.Join(uniqueExpressions, " union all\n    ")

	// get 1-1
	matchingSQLs := lo.Map(uniqueCols, func(col iop.Column, i int) string {
		template := `select uv.unique_column_key_1,	'{col_key}' as unique_column_key_2	from unique_values uv
				inner join {schema}.{table} t on cast(t.{field} as {string_type}) = uv.val`
		return g.R(
			template,
			"col_key", col.Key(),
			"field", da.Conn.Quote(col.Name),
			"string_type", stringType,
			"schema", da.Conn.Quote(col.Schema),
			"table", da.Conn.Quote(col.Table),
		)
	})
	matchingSQL := strings.Join(matchingSQLs, "    union all\n    ")

	// run
	sql := g.R(`with unique_values as (
				{non_unique_sql}
			)
			, matching as (
				{matching_sql}
			)
			select unique_column_key_1, unique_column_key_2
			from matching
			where unique_column_key_2 != unique_column_key_1
			order by unique_column_key_1
			`,
		"non_unique_sql", nonUniqueSQL,
		"matching_sql", matchingSQL,
	)
	data, err := da.Conn.Query(sql)
	if err != nil {
		return g.Error(err, "could not get matching columns")
	}

	for _, rec := range data.Records() {
		uniqueColumnKey1 := cast.ToString(rec["unique_column_key_1"])
		uniqueColumnKey2 := cast.ToString(rec["unique_column_key_2"])

		uniqueColumn1 := da.ColumnMap[uniqueColumnKey1]
		uniqueColumn2 := da.ColumnMap[uniqueColumnKey2]

		table1 := g.F("%s.%s", uniqueColumn1.Schema, uniqueColumn1.Table)
		table2 := g.F("%s.%s", uniqueColumn2.Schema, uniqueColumn2.Table)

		// one to one
		if mt, ok := da.RelationMap[table1]; ok {
			if m, ok := mt[uniqueColumnKey1]; ok {
				m[uniqueColumnKey2] = RelationOneToOne
			} else {
				mt[uniqueColumnKey1] = map[string]Relation{uniqueColumnKey2: RelationOneToOne}
			}
		} else {
			da.RelationMap[table1] = map[string]map[string]Relation{
				uniqueColumnKey1: {uniqueColumnKey2: RelationOneToOne},
			}
		}

		// one to one
		if mt, ok := da.RelationMap[table2]; ok {
			if m, ok := mt[uniqueColumnKey2]; ok {
				m[uniqueColumnKey1] = RelationOneToOne
			} else {
				mt[uniqueColumnKey2] = map[string]Relation{uniqueColumnKey1: RelationOneToOne}
			}
		} else {
			da.RelationMap[table2] = map[string]map[string]Relation{
				uniqueColumnKey2: {uniqueColumnKey1: RelationOneToOne},
			}
		}
	}
	return
}

func (da *DataAnalyzer) GetManyToMany(nonUniqueCols iop.Columns) (err error) {
	return nil
}
