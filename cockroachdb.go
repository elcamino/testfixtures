package testfixtures

import (
	"database/sql"
	"fmt"
	"strings"
)

type cockroachDB struct {
	baseHelper

	useAlterConstraint bool
	useDropConstraint  bool
	skipResetSequences bool
	resetSequencesTo   int64

	tables                   []string
	sequences                []string
	nonDeferrableConstraints []pgConstraint
	constraints              []pgConstraint
	tablesChecksum           map[string]string
}

func (h *cockroachDB) init(db *sql.DB) error {
	var err error

	h.tables, err = h.tableNames(db)
	if err != nil {
		return err
	}

	h.sequences, err = h.getSequences(db)
	if err != nil {
		return err
	}

	h.nonDeferrableConstraints, err = h.getNonDeferrableConstraints(db)
	if err != nil {
		return err
	}

	h.constraints, err = h.getConstraints(db)
	if err != nil {
		return err
	}

	return nil
}

func (*cockroachDB) paramType() int {
	return paramTypeDollar
}

func (*cockroachDB) databaseName(q queryable) (string, error) {
	var dbName string
	err := q.QueryRow("SELECT current_database()").Scan(&dbName)
	return dbName, err
}

func (h *cockroachDB) tableNames(q queryable) ([]string, error) {
	var tables []string

	const sql = `
	        SELECT pg_namespace.nspname || '.' || pg_class.relname
		FROM pg_class
		INNER JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		WHERE pg_class.relkind = 'r'
		  AND pg_namespace.nspname NOT IN ('pg_catalog', 'information_schema', 'crdb_internal')
		  AND pg_namespace.nspname NOT LIKE 'pg_toast%'
		  AND pg_namespace.nspname NOT LIKE '\_timescaledb%';
	`
	rows, err := q.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

func (h *cockroachDB) getSequences(q queryable) ([]string, error) {
	const sql = `
		SELECT pg_namespace.nspname || '.' || pg_class.relname AS sequence_name
		FROM pg_class
		INNER JOIN pg_namespace ON pg_namespace.oid = pg_class.relnamespace
		WHERE pg_class.relkind = 'S'
		  AND pg_namespace.nspname NOT LIKE '\_timescaledb%'
	`

	rows, err := q.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sequences []string
	for rows.Next() {
		var sequence string
		if err = rows.Scan(&sequence); err != nil {
			return nil, err
		}
		sequences = append(sequences, sequence)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return sequences, nil
}

func (*cockroachDB) getNonDeferrableConstraints(q queryable) ([]pgConstraint, error) {
	var constraints []pgConstraint

	const sql = `
		SELECT table_schema || '.' || table_name, constraint_name
		FROM information_schema.table_constraints
		WHERE constraint_type = 'FOREIGN KEY'
		  AND is_deferrable = 'NO'
		  AND table_schema <> 'crdb_internal'
		  AND table_schema NOT LIKE '\_timescaledb%'
  	`
	rows, err := q.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var constraint pgConstraint
		if err = rows.Scan(&constraint.tableName, &constraint.constraintName); err != nil {
			return nil, err
		}
		constraints = append(constraints, constraint)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return constraints, nil
}

func (h *cockroachDB) getConstraints(q queryable) ([]pgConstraint, error) {
	var constraints []pgConstraint

	const sql = `
		SELECT conrelid::regclass AS table_from, conname, pg_get_constraintdef(pg_constraint.oid)
		FROM pg_constraint
		INNER JOIN pg_namespace ON pg_namespace.oid = pg_constraint.connamespace
		WHERE contype = 'f'
		  AND pg_namespace.nspname NOT IN ('pg_catalog', 'information_schema', 'crdb_internal')
		  AND pg_namespace.nspname NOT LIKE 'pg_toast%'
		  AND pg_namespace.nspname NOT LIKE '\_timescaledb%';
		`
	rows, err := q.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var constraint pgConstraint
		if err = rows.Scan(
			&constraint.tableName,
			&constraint.constraintName,
			&constraint.definition,
		); err != nil {
			return nil, err
		}
		constraints = append(constraints, constraint)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	return constraints, nil
}

func (h *cockroachDB) dropAndRecreateConstraints(db *sql.DB, loadFn loadFunction) (err error) {
	defer func() {
		// Re-create constraints again after load
		var b strings.Builder
		for _, constraint := range h.constraints {
			b.WriteString(fmt.Sprintf(
				"ALTER TABLE %s ADD CONSTRAINT %s %s;",
				h.quoteKeyword(constraint.tableName),
				h.quoteKeyword(constraint.constraintName),
				constraint.definition,
			))
		}
		if _, err2 := db.Exec(b.String()); err2 != nil && err == nil {
			err = err2
		}
	}()

	var b strings.Builder
	for _, constraint := range h.constraints {
		b.WriteString(fmt.Sprintf(
			"ALTER TABLE %s DROP CONSTRAINT %s;",
			h.quoteKeyword(constraint.tableName),
			h.quoteKeyword(constraint.constraintName),
		))
	}
	if _, err := db.Exec(b.String()); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if err = loadFn(tx); err != nil {
		return err
	}

	return tx.Commit()
}

func (h *cockroachDB) disableTriggers(db *sql.DB, loadFn loadFunction) (err error) {
	err = nil
	return
}

func (h *cockroachDB) makeConstraintsDeferrable(db *sql.DB, loadFn loadFunction) (err error) {
	defer func() {
		// ensure constraint being not deferrable again after load
		var b strings.Builder
		for _, constraint := range h.nonDeferrableConstraints {
			b.WriteString(fmt.Sprintf("ALTER TABLE %s ALTER CONSTRAINT %s NOT DEFERRABLE;", h.quoteKeyword(constraint.tableName), h.quoteKeyword(constraint.constraintName)))
		}
		if _, err2 := db.Exec(b.String()); err2 != nil && err == nil {
			err = err2
		}
	}()

	var b strings.Builder
	for _, constraint := range h.nonDeferrableConstraints {
		b.WriteString(fmt.Sprintf("ALTER TABLE %s ALTER CONSTRAINT %s DEFERRABLE;", h.quoteKeyword(constraint.tableName), h.quoteKeyword(constraint.constraintName)))
	}
	if _, err := db.Exec(b.String()); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.Exec("SET CONSTRAINTS ALL DEFERRED"); err != nil {
		return err
	}

	if err = loadFn(tx); err != nil {
		return err
	}

	return tx.Commit()
}

func (h *cockroachDB) disableReferentialIntegrity(db *sql.DB, loadFn loadFunction) (err error) {
	// ensure sequences being reset after load
	if !h.skipResetSequences {
		defer func() {
			if err2 := h.resetSequences(db); err2 != nil && err == nil {
				err = err2
			}
		}()
	}

	if h.useDropConstraint {
		return h.dropAndRecreateConstraints(db, loadFn)
	}
	if h.useAlterConstraint {
		return h.makeConstraintsDeferrable(db, loadFn)
	}
	return h.disableTriggers(db, loadFn)
}

func (h *cockroachDB) resetSequences(db *sql.DB) error {
	resetSequencesTo := h.resetSequencesTo
	if resetSequencesTo == 0 {
		resetSequencesTo = 10000
	}

	for _, sequence := range h.sequences {
		_, err := db.Exec(fmt.Sprintf("SELECT SETVAL('%s', %d)", sequence, resetSequencesTo))
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *cockroachDB) isTableModified(q queryable, tableName string) (bool, error) {
	checksum, err := h.getChecksum(q, tableName)
	if err != nil {
		return false, err
	}

	oldChecksum := h.tablesChecksum[tableName]

	return oldChecksum == "" || checksum != oldChecksum, nil
}

func (h *cockroachDB) afterLoad(q queryable) error {
	if h.tablesChecksum != nil {
		return nil
	}

	h.tablesChecksum = make(map[string]string, len(h.tables))
	for _, t := range h.tables {
		checksum, err := h.getChecksum(q, t)
		if err != nil {
			return err
		}
		h.tablesChecksum[t] = checksum
	}
	return nil
}

func (h *cockroachDB) getChecksum(q queryable, tableName string) (string, error) {
	sqlStr := fmt.Sprintf(`
			SELECT md5(CAST((json_agg(t.*)) AS TEXT))
			FROM %s AS t
		`,
		h.quoteKeyword(tableName),
	)

	var checksum sql.NullString
	if err := q.QueryRow(sqlStr).Scan(&checksum); err != nil {
		return "", err
	}
	return checksum.String, nil
}

func (*cockroachDB) quoteKeyword(s string) string {
	parts := strings.Split(s, ".")
	for i, p := range parts {
		parts[i] = fmt.Sprintf(`"%s"`, p)
	}
	return strings.Join(parts, ".")
}
