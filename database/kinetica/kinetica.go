//go:build go1.9
// +build go1.9

package kinetica

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/hashicorp/go-multierror"
	"io"
	nurl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.uber.org/atomic"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/multistmt"
	"github.com/lib/pq"
)

func init() {
	db := Kinetica{}
	database.Register("kinetica", &db)
}

var (
	multiStmtDelimiter = []byte(";")

	DefaultMigrationsTable       = "schema_migrations"
	DefaultLockTable             = "schema_lock"
	DefaultMultiStatementMaxSize = 10 * 1 << 20 // 10 MB
)

var (
	ErrNilConfig      = fmt.Errorf("no config")
	ErrNoDatabaseName = fmt.Errorf("no database name")
	ErrNoSchema       = fmt.Errorf("no schema")
	ErrDatabaseDirty  = fmt.Errorf("database is dirty")
)

type Config struct {
	MigrationsTable       string
	LockTable             string
	ForceLock             bool
	MigrationsTableQuoted bool
	MultiStatementEnabled bool
	DatabaseName          string
	SchemaName            string
	migrationsSchemaName  string
	migrationsTableName   string
	StatementTimeout      time.Duration
	MultiStatementMaxSize int
}

type Kinetica struct {
	// Locking and unlocking need to use the same connection
	conn     *sql.Conn
	db       *sql.DB
	isLocked atomic.Bool

	// Open and WithInstance need to guarantee that config is never nil
	config *Config
}

func WithConnection(ctx context.Context, conn *sql.Conn, config *Config) (*Kinetica, error) {
	if config == nil {
		return nil, ErrNilConfig
	}

	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}

	if config.DatabaseName == "" {
		query := `SELECT CURRENT_DATABASE()`
		var databaseName string
		if err := conn.QueryRowContext(ctx, query).Scan(&databaseName); err != nil {
			return nil, &database.Error{OrigErr: err, Query: []byte(query)}
		}

		if len(databaseName) == 0 {
			return nil, ErrNoDatabaseName
		}

		config.DatabaseName = databaseName
	}

	if config.SchemaName == "" {
		query := `SELECT CURRENT_SCHEMA()`
		var schemaName sql.NullString
		if err := conn.QueryRowContext(ctx, query).Scan(&schemaName); err != nil {
			return nil, &database.Error{OrigErr: err, Query: []byte(query)}
		}

		if !schemaName.Valid {
			return nil, ErrNoSchema
		}

		config.SchemaName = schemaName.String
	}

	if len(config.MigrationsTable) == 0 {
		config.MigrationsTable = DefaultMigrationsTable
	}

	if len(config.LockTable) == 0 {
		config.LockTable = DefaultLockTable
	}

	config.migrationsSchemaName = config.SchemaName
	config.migrationsTableName = config.MigrationsTable
	if config.MigrationsTableQuoted {
		re := regexp.MustCompile(`"(.*?)"`)
		result := re.FindAllStringSubmatch(config.MigrationsTable, -1)
		config.migrationsTableName = result[len(result)-1][1]
		if len(result) == 2 {
			config.migrationsSchemaName = result[0][1]
		} else if len(result) > 2 {
			return nil, fmt.Errorf("\"%s\" MigrationsTable contains too many dot characters", config.MigrationsTable)
		}
	}

	px := &Kinetica{
		conn:   conn,
		config: config,
	}

	if err := px.ensureLockTable(); err != nil {
		return nil, err
	}

	if err := px.ensureVersionTable(); err != nil {
		return nil, err
	}

	return px, nil
}

func WithInstance(instance *sql.DB, config *Config) (database.Driver, error) {
	ctx := context.Background()

	if err := instance.Ping(); err != nil {
		return nil, err
	}

	conn, err := instance.Conn(ctx)
	if err != nil {
		return nil, err
	}

	px, err := WithConnection(ctx, conn, config)
	if err != nil {
		return nil, err
	}
	px.db = instance
	return px, nil
}

func (p *Kinetica) Open(url string) (database.Driver, error) {
	purl, err := nurl.Parse(url)
	if err != nil {
		return nil, err
	}

	// As Kinetica uses the postgres protocol, and 'postgres' is already a registered database, we need to replace the
	// connect prefix, with the actual protocol, so that the library can differentiate between the implementations
	re := regexp.MustCompile("^(kinetica)")
	connectString := re.ReplaceAllString(migrate.FilterCustomQuery(purl).String(), "postgres")

	db, err := sql.Open("postgres", connectString)
	if err != nil {
		return nil, err
	}

	migrationsTable := purl.Query().Get("x-migrations-table")
	migrationsTableQuoted := false
	if s := purl.Query().Get("x-migrations-table-quoted"); len(s) > 0 {
		migrationsTableQuoted, err = strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("Unable to parse option x-migrations-table-quoted: %w", err)
		}
	}
	if (len(migrationsTable) > 0) && (migrationsTableQuoted) && ((migrationsTable[0] != '"') || (migrationsTable[len(migrationsTable)-1] != '"')) {
		return nil, fmt.Errorf("x-migrations-table must be quoted (for instance '\"migrate\".\"schema_migrations\"') when x-migrations-table-quoted is enabled, current value is: %s", migrationsTable)
	}

	lockTable := purl.Query().Get("x-lock-table")
	if len(lockTable) == 0 {
		lockTable = DefaultLockTable
	}

	forceLockQuery := purl.Query().Get("x-force-lock")
	forceLock, err := strconv.ParseBool(forceLockQuery)
	if err != nil {
		forceLock = false
	}

	statementTimeoutString := purl.Query().Get("x-statement-timeout")
	statementTimeout := 0
	if statementTimeoutString != "" {
		statementTimeout, err = strconv.Atoi(statementTimeoutString)
		if err != nil {
			return nil, err
		}
	}

	multiStatementMaxSize := DefaultMultiStatementMaxSize
	if s := purl.Query().Get("x-multi-statement-max-size"); len(s) > 0 {
		multiStatementMaxSize, err = strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		if multiStatementMaxSize <= 0 {
			multiStatementMaxSize = DefaultMultiStatementMaxSize
		}
	}

	multiStatementEnabled := false
	if s := purl.Query().Get("x-multi-statement"); len(s) > 0 {
		multiStatementEnabled, err = strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("Unable to parse option x-multi-statement: %w", err)
		}
	}

	px, err := WithInstance(db, &Config{
		DatabaseName:          purl.Path,
		MigrationsTable:       migrationsTable,
		MigrationsTableQuoted: migrationsTableQuoted,
		LockTable:             lockTable,
		ForceLock:             forceLock,
		StatementTimeout:      time.Duration(statementTimeout) * time.Millisecond,
		MultiStatementEnabled: multiStatementEnabled,
		MultiStatementMaxSize: multiStatementMaxSize,
	})

	if err != nil {
		return nil, err
	}

	return px, nil
}

func (p *Kinetica) Close() error {
	connErr := p.conn.Close()
	var dbErr error
	if p.db != nil {
		dbErr = p.db.Close()
	}

	if connErr != nil || dbErr != nil {
		return fmt.Errorf("conn: %v, db: %v", connErr, dbErr)
	}
	return nil
}

func (p *Kinetica) Lock() error {
	/*
		return database.CasRestoreOnErr(&p.isLocked, false, true, database.ErrLocked, func() (err error) {
			return crdb.ExecuteTx(context.Background(), p.db, nil, func(tx *sql.Tx) (err error) {
				aid, err := database.GenerateAdvisoryLockId(p.config.DatabaseName)
				if err != nil {
					return err
				}

				query := "SELECT * FROM " + p.config.LockTable + " WHERE lock_id = $1"
				rows, err := tx.Query(query, aid)
				if err != nil {
					return database.Error{OrigErr: err, Err: "failed to fetch migration lock", Query: []byte(query)}
				}
				defer func() {
					if errClose := rows.Close(); errClose != nil {
						err = multierror.Append(err, errClose)
					}
				}()

				// If row exists at all, lock is present
				locked := rows.Next()
				if locked && !p.config.ForceLock {
					return database.ErrLocked
				}

				query = "INSERT INTO " + p.config.LockTable + " (lock_id) VALUES ($1)"
				if _, err := tx.Exec(query, aid); err != nil {
					return database.Error{OrigErr: err, Err: "failed to set migration lock", Query: []byte(query)}
				}

				return nil
			})
		})
	*/

	return nil
}

func (p *Kinetica) Unlock() error {
	/*
		return database.CasRestoreOnErr(&p.isLocked, true, false, database.ErrNotLocked, func() (err error) {
			aid, err := database.GenerateAdvisoryLockId(p.config.DatabaseName)
			if err != nil {
				return err
			}

			// In the event of an implementation (non-migration) error, it is possible for the lock to not be released.  Until
			// a better locking mechanism is added, a manual purging of the lock table may be required in such circumstances
			query := "DELETE FROM " + p.config.LockTable + " WHERE lock_id = $1"
			if _, err := p.conn.ExecContext(context.Background(), query, aid); err != nil {
				if e, ok := err.(*pq.Error); ok {
					// 42P01 is "UndefinedTableError" in CockroachDB
					// https://github.com/cockroachdb/cockroach/blob/master/pkg/sql/pgwire/pgerror/codes.go
					if e.Code == "42P01" {
						// On drops, the lock table is fully removed;  This is fine, and is a valid "unlocked" state for the schema
						return nil
					}
				}

				return database.Error{OrigErr: err, Err: "failed to release migration lock", Query: []byte(query)}
			}

			return nil
		})
	*/

	return nil
}

func (p *Kinetica) Run(migration io.Reader) error {
	if p.config.MultiStatementEnabled {
		var err error
		if e := multistmt.Parse(migration, multiStmtDelimiter, p.config.MultiStatementMaxSize, func(m []byte) bool {
			if err = p.runStatement(m); err != nil {
				return false
			}
			return true
		}); e != nil {
			return e
		}
		return err
	}
	migr, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	return p.runStatement(migr)
}

func (p *Kinetica) runStatement(statement []byte) error {
	ctx := context.Background()
	if p.config.StatementTimeout != 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.config.StatementTimeout)
		defer cancel()
	}
	query := string(statement)
	if strings.TrimSpace(query) == "" {
		return nil
	}
	if _, err := p.conn.ExecContext(ctx, query); err != nil {
		if pgErr, ok := err.(*pq.Error); ok {
			var line uint
			var col uint
			var lineColOK bool
			if pgErr.Position != "" {
				if pos, err := strconv.ParseUint(pgErr.Position, 10, 64); err == nil {
					line, col, lineColOK = computeLineFromPos(query, int(pos))
				}
			}
			message := fmt.Sprintf("migration failed: %s", pgErr.Message)
			if lineColOK {
				message = fmt.Sprintf("%s (column %d)", message, col)
			}
			if pgErr.Detail != "" {
				message = fmt.Sprintf("%s, %s", message, pgErr.Detail)
			}
			return database.Error{OrigErr: err, Err: message, Query: statement, Line: line}
		}
		return database.Error{OrigErr: err, Err: "migration failed", Query: statement}
	}
	return nil
}

func computeLineFromPos(s string, pos int) (line uint, col uint, ok bool) {
	// replace crlf with lf
	s = strings.Replace(s, "\r\n", "\n", -1)
	// pg docs: pos uses index 1 for the first character, and positions are measured in characters not bytes
	runes := []rune(s)
	if pos > len(runes) {
		return 0, 0, false
	}
	sel := runes[:pos]
	line = uint(runesCount(sel, newLine) + 1)
	col = uint(pos - 1 - runesLastIndex(sel, newLine))
	return line, col, true
}

const newLine = '\n'

func runesCount(input []rune, target rune) int {
	var count int
	for _, r := range input {
		if r == target {
			count++
		}
	}
	return count
}

func runesLastIndex(input []rune, target rune) int {
	for i := len(input) - 1; i >= 0; i-- {
		if input[i] == target {
			return i
		}
	}
	return -1
}

func (p *Kinetica) SetVersion(version int, dirty bool) error {
	// Clear the table of any versions before inserting latest version.
	query := `TRUNCATE TABLE ` + pq.QuoteIdentifier(p.config.migrationsSchemaName) + `.` + pq.QuoteIdentifier(p.config.migrationsTableName)

	_, err := p.conn.ExecContext(context.Background(), query)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	// Also re-write the schema version for nil dirty versions to prevent
	// empty schema version for failed down migration on the first migration
	// See: https://github.com/golang-migrate/migrate/issues/330
	if version >= 0 || (version == database.NilVersion && dirty) {
		// NOTE: Had to use manual params here as placeholders didnt work...  Probably a driver nuiance with kinetica
		query = `INSERT INTO ` + pq.QuoteIdentifier(p.config.migrationsSchemaName) + `.` + pq.QuoteIdentifier(p.config.migrationsTableName) + ` (version, dirty) VALUES (` + strconv.Itoa(version) + `,` + strconv.FormatBool(dirty) + `)`
		_, err = p.conn.ExecContext(context.Background(), query)
		if err != nil {
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}

	return nil
}

func (p *Kinetica) Version() (version int, dirty bool, err error) {
	query := `SELECT version, dirty FROM ` + pq.QuoteIdentifier(p.config.migrationsSchemaName) + `.` + pq.QuoteIdentifier(p.config.migrationsTableName) + ` LIMIT 1`
	err = p.conn.QueryRowContext(context.Background(), query).Scan(&version, &dirty)
	switch {
	case err == sql.ErrNoRows:
		return database.NilVersion, false, nil

	case err != nil:
		if e, ok := err.(*pq.Error); ok {
			if e.Code.Name() == "undefined_table" {
				return database.NilVersion, false, nil
			}
		}
		return 0, false, &database.Error{OrigErr: err, Query: []byte(query)}

	default:
		return version, dirty, nil
	}
}

func (p *Kinetica) Drop() (err error) {
	// select all tables in current schema
	query := `SELECT table_name FROM information_schema.tables WHERE table_schema=(SELECT current_schema()) AND table_type='BASE TABLE'`
	tables, err := p.conn.QueryContext(context.Background(), query)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	defer func() {
		if errClose := tables.Close(); errClose != nil {
			err = multierror.Append(err, errClose)
		}
	}()

	// delete one table after another
	tableNames := make([]string, 0)
	for tables.Next() {
		var tableName string
		if err := tables.Scan(&tableName); err != nil {
			return err
		}
		if len(tableName) > 0 {
			tableNames = append(tableNames, tableName)
		}
	}
	if err := tables.Err(); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if len(tableNames) > 0 {
		// delete one by one ...
		for _, t := range tableNames {
			query = `DROP TABLE IF EXISTS ` + pq.QuoteIdentifier(t) + ` CASCADE`
			if _, err := p.conn.ExecContext(context.Background(), query); err != nil {
				return &database.Error{OrigErr: err, Query: []byte(query)}
			}
		}
	}

	return nil
}

// ensureVersionTable checks if versions table exists and, if not, creates it.
// Note that this function locks the database, which deviates from the usual
// convention of "caller locks" in the Kinetica type.
func (p *Kinetica) ensureVersionTable() (err error) {
	if err = p.Lock(); err != nil {
		return err
	}

	defer func() {
		if e := p.Unlock(); e != nil {
			if err == nil {
				err = e
			} else {
				err = multierror.Append(err, e)
			}
		}
	}()

	query := `SELECT COUNT(1) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2 LIMIT 1`
	row := p.conn.QueryRowContext(context.Background(), query, p.config.migrationsSchemaName, p.config.migrationsTableName)

	var count int
	err = row.Scan(&count)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if count == 1 {
		return nil
	}

	fmt.Println("The migrations table is missing.  Attempting to create...")

	query = `CREATE TABLE IF NOT EXISTS ` + pq.QuoteIdentifier(p.config.migrationsSchemaName) + `.` + pq.QuoteIdentifier(p.config.migrationsTableName) + ` (version bigint not null primary key, dirty boolean not null)`
	if _, err = p.conn.ExecContext(context.Background(), query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	return nil
}

func (p *Kinetica) ensureLockTable() error {
	query := `SELECT COUNT(1) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2 LIMIT 1`
	row := p.conn.QueryRowContext(context.Background(), query, p.config.migrationsSchemaName, p.config.LockTable)

	var count int
	err := row.Scan(&count)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if count == 1 {
		return nil
	}

	fmt.Println("The lock table is missing.  Attempting to create...")

	query = `CREATE TABLE IF NOT EXISTS ` + pq.QuoteIdentifier(p.config.migrationsSchemaName) + `.` + pq.QuoteIdentifier(p.config.LockTable) + ` (lock_id INT NOT NULL PRIMARY KEY)`
	if _, err = p.conn.ExecContext(context.Background(), query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	return nil
}
