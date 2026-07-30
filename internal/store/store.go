package store

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
)

const (
	defaultBusyTimeout = 500 * time.Millisecond
	defaultBusyRetries = 4
	motusApplicationID = 0x4d4f5455 // "MOTU"
)

var (
	errSchemaMigrationRequired   = errors.New("store: schema migration required")
	errJournalMigrationRequired  = errors.New("store: journal mode migration required")
	errIdentityMigrationRequired = errors.New("store: identity marker migration required")
	errRollbackRecoveryRequired  = errors.New("store: rollback recovery required")
)

type Store struct {
	db          *sql.DB
	stateDir    string
	database    string
	readOnly    bool
	busyRetries int
	now         func() time.Time
}

// Open opens or creates the current ledger in stateDir. The directory is
// treated as private application state and the database has the fixed name
// DatabaseFilename.
func Open(ctx context.Context, stateDir string) (*Store, error) {
	return open(ctx, stateDir, false, false)
}

// OpenReadOnly opens an existing current-schema, rollback-journal ledger
// without creating or migrating it. It fails with ErrNotFound when the fixed
// database is absent.
func OpenReadOnly(ctx context.Context, stateDir string) (*Store, error) {
	return open(ctx, stateDir, true, true)
}

// OpenForRead opens an existing ledger without creating one. A valid older
// schema or WAL-mode ledger is migrated before the ledger is reopened
// read-only.
func OpenForRead(ctx context.Context, stateDir string) (*Store, error) {
	ledger, err := OpenReadOnly(ctx, stateDir)
	schemaMigration := errors.Is(err, errSchemaMigrationRequired)
	journalMigration := errors.Is(err, errJournalMigrationRequired)
	identityMigration := errors.Is(err, errIdentityMigrationRequired)
	rollbackRecovery := errors.Is(err, sqlite3.READONLY_ROLLBACK) ||
		errors.Is(err, sqlite3.READONLY_RECOVERY) ||
		errors.Is(err, errRollbackRecoveryRequired)
	if !schemaMigration && !journalMigration && !identityMigration && !rollbackRecovery {
		return ledger, err
	}
	migrator, err := open(ctx, stateDir, false, true)
	if err != nil {
		if errors.Is(err, ErrInvalidSchema) {
			return nil, err
		}
		if journalMigration {
			if errors.Is(err, errJournalMigrationRequired) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: the ledger uses WAL and needs exclusive writable access; stop every Motus process using this state directory, make the directory and its files writable, then retry: %w",
				errJournalMigrationRequired, err)
		}
		if rollbackRecovery {
			return nil, fmt.Errorf("%w: an interrupted write left a rollback journal; stop every Motus process using this state directory, make the directory and its files writable, then retry: %w",
				errRollbackRecoveryRequired, err)
		}
		return nil, fmt.Errorf("migrate ledger: %w", err)
	}
	if err := migrator.Close(); err != nil {
		return nil, fmt.Errorf("close migrated ledger: %w", err)
	}
	return OpenReadOnly(ctx, stateDir)
}

func open(ctx context.Context, stateDir string, readOnly, existingOnly bool) (*Store, error) {
	dir, database, err := prepareStatePath(stateDir, existingOnly)
	if err != nil {
		return nil, err
	}
	rollbackJournal, err := hasRollbackJournal(database)
	if err != nil {
		return nil, err
	}
	if rollbackJournal {
		header, err := inspectSQLiteHeader(database)
		if err != nil {
			return nil, err
		}
		if header.valid && header.applicationID != 0 && header.applicationID != motusApplicationID {
			return nil, fmt.Errorf("%w: database %q has an unexpected application identity and a rollback journal",
				ErrInvalidSchema, database)
		}
		if readOnly && (!header.valid || header.applicationID != motusApplicationID) {
			return nil, fmt.Errorf("%w: database %q needs writable validation and rollback recovery",
				errRollbackRecoveryRequired, database)
		}
		if !readOnly {
			if err := validateRollbackSnapshot(ctx, dir, database); err != nil {
				return nil, err
			}
		}
	}
	journalMigration, err := requiresJournalMigration(database)
	if err != nil {
		return nil, err
	}
	if journalMigration {
		if readOnly {
			return nil, fmt.Errorf("%w: database %q uses WAL", errJournalMigrationRequired, database)
		}
		if err := migrateWALJournal(ctx, dir, database); err != nil {
			if errors.Is(err, ErrInvalidSchema) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: the ledger uses WAL and needs exclusive writable access; stop every Motus process using this state directory, make the directory and its files writable, then retry: %w",
				errJournalMigrationRequired, err)
		}
	}

	dsn := sqliteDSN(database, readOnly, defaultBusyTimeout)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite driver: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)

	s := &Store{
		db:          db,
		stateDir:    dir,
		database:    database,
		readOnly:    readOnly,
		busyRetries: defaultBusyRetries,
		now: func() time.Time {
			return time.Now().UTC().Round(0)
		},
	}
	if err := s.pingWithRetry(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open ledger database: %w", err)
	}
	if err := verifyStatePath(dir, database); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.verifyPragmas(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if readOnly {
		if err := validateSchema(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	} else if err := s.initializeSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func sqliteDSN(database string, readOnly bool, busyTimeout time.Duration) string {
	journalMode := ""
	if !readOnly {
		// TRUNCATE preserves rollback-journal durability without deleting the
		// journal at every commit. Non-WAL modes are connection-scoped, so each
		// writable connection must request it.
		journalMode = "TRUNCATE"
	}
	return sqliteDSNWithJournalMode(database, readOnly, busyTimeout, journalMode)
}

func sqliteDSNWithJournalMode(
	database string,
	readOnly bool,
	busyTimeout time.Duration,
	journalMode string,
) string {
	u := sqliteFileURL(database, runtime.GOOS == "windows")
	q := u.Query()
	// Install the busy handler before pragmas such as journal_mode that may
	// contend when multiple processes open a new ledger at the same time.
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busyTimeout.Milliseconds(), 10)+")")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(ON)")
	} else {
		// prepareStatePath creates the file first, so mode=rw prevents the driver
		// from following a late missing-path condition by creating elsewhere.
		q.Set("mode", "rw")
		if journalMode != "" {
			q.Add("_pragma", "journal_mode("+journalMode+")")
		}
		q.Set("_txlock", "immediate")
	}
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "recursive_triggers(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "trusted_schema(OFF)")
	q.Set("_dqs", "false")
	u.RawQuery = q.Encode()
	return u.String()
}

func sqliteFileURL(database string, windows bool) *url.URL {
	path := filepath.ToSlash(database)
	if windows {
		// filepath.ToSlash follows the host platform, so portable tests need an
		// explicit replacement when exercising Windows paths elsewhere.
		path = strings.ReplaceAll(database, `\`, "/")
		// SQLite interprets file:///C:/... as /C:/... with this driver. Keep a
		// local drive path opaque so SQLite receives file:C:/..., matching the
		// driver's documented filepath.ToSlash construction. prepareStatePath
		// rejects Windows network and device paths before this point.
		return &url.URL{Scheme: "file", Opaque: escapeSQLiteURIPath(path)}
	}
	return &url.URL{Scheme: "file", Path: path}
}

func escapeSQLiteURIPath(path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func requiresJournalMigration(database string) (bool, error) {
	header, err := inspectSQLiteHeader(database)
	if err != nil {
		return false, err
	}
	if !header.valid {
		return false, nil
	}
	// SQLite file-header bytes 18 and 19 are both 2 for WAL databases and 1
	// for rollback-journal databases. Treat either WAL marker as requiring a
	// writable migration so a partially changed header is never opened as a
	// sidecar-free read-only ledger.
	return header.writeVersion == 2 || header.readVersion == 2, nil
}

type sqliteHeader struct {
	valid         bool
	writeVersion  byte
	readVersion   byte
	applicationID uint32
}

func inspectSQLiteHeader(database string) (sqliteHeader, error) {
	file, err := os.Open(database)
	if err != nil {
		return sqliteHeader{}, fmt.Errorf("inspect ledger header: %w", err)
	}
	defer file.Close()

	var header [72]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return sqliteHeader{}, nil
		}
		return sqliteHeader{}, fmt.Errorf("inspect ledger header: %w", err)
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return sqliteHeader{}, nil
	}
	return sqliteHeader{
		valid:         true,
		writeVersion:  header[18],
		readVersion:   header[19],
		applicationID: binary.BigEndian.Uint32(header[68:72]),
	}, nil
}

func hasMotusApplicationID(database string) (bool, error) {
	header, err := inspectSQLiteHeader(database)
	if err != nil {
		return false, err
	}
	return header.valid && header.applicationID == motusApplicationID, nil
}

func prepareStatePath(stateDir string, existingOnly bool) (string, string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", "", fmt.Errorf("%w: empty state directory", ErrInvalid)
	}
	abs, err := filepath.Abs(filepath.Clean(stateDir))
	if err != nil {
		return "", "", fmt.Errorf("resolve state directory: %w", err)
	}
	if err := validatePlatformStatePath(abs); err != nil {
		return "", "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect state directory: %w", err)
		}
		if existingOnly {
			return "", "", fmt.Errorf("%w: database %q", ErrNotFound, filepath.Join(abs, DatabaseFilename))
		}
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return "", "", fmt.Errorf("create private state directory: %w", err)
		}
		info, err = os.Lstat(abs)
		if err != nil {
			return "", "", fmt.Errorf("inspect created state directory: %w", err)
		}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("%w: state directory is a symlink", ErrUnsafeStatePath)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("%w: state path is not a directory", ErrUnsafeStatePath)
	}
	if err := requirePrivateMode(info, true); err != nil {
		return "", "", err
	}
	// Resolve any symlinks in ancestor components after proving that the state
	// directory itself is not a symlink. All subsequent opens use this physical
	// path, so replacing an ancestor symlink cannot redirect the database.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", fmt.Errorf("resolve state directory ancestors: %w", err)
	}
	abs = filepath.Clean(resolved)
	info, err = os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("%w: resolved state directory is unsafe", ErrUnsafeStatePath)
	}
	if err := requirePrivateMode(info, true); err != nil {
		return "", "", err
	}

	database := filepath.Join(abs, DatabaseFilename)
	dbInfo, err := os.Lstat(database)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect database path: %w", err)
		}
		if existingOnly {
			return "", "", fmt.Errorf("%w: database %q", ErrNotFound, database)
		}
		f, createErr := os.OpenFile(database, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			if !errors.Is(createErr, os.ErrExist) {
				return "", "", fmt.Errorf("create private database file: %w", createErr)
			}
			dbInfo, err = os.Lstat(database)
			if err != nil {
				return "", "", fmt.Errorf("inspect concurrently created database file: %w", err)
			}
		} else {
			if closeErr := f.Close(); closeErr != nil {
				return "", "", fmt.Errorf("close new database file: %w", closeErr)
			}
			dbInfo, err = os.Lstat(database)
			if err != nil {
				return "", "", fmt.Errorf("inspect created database file: %w", err)
			}
		}
	}
	if dbInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("%w: database file is a symlink", ErrUnsafeStatePath)
	}
	if !dbInfo.Mode().IsRegular() {
		return "", "", fmt.Errorf("%w: database path is not a regular file", ErrUnsafeStatePath)
	}
	if err := requirePrivateMode(dbInfo, false); err != nil {
		return "", "", err
	}
	return abs, database, nil
}

func verifyStatePath(stateDir, database string) error {
	dirInfo, err := os.Lstat(stateDir)
	if err != nil {
		return fmt.Errorf("reinspect state directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return fmt.Errorf("%w: state directory changed during open", ErrUnsafeStatePath)
	}
	if err := requirePrivateMode(dirInfo, true); err != nil {
		return err
	}
	dbInfo, err := os.Lstat(database)
	if err != nil {
		return fmt.Errorf("reinspect database path: %w", err)
	}
	if dbInfo.Mode()&os.ModeSymlink != 0 || !dbInfo.Mode().IsRegular() {
		return fmt.Errorf("%w: database path changed during open", ErrUnsafeStatePath)
	}
	return requirePrivateMode(dbInfo, false)
}

func requirePrivateMode(info os.FileInfo, directory bool) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if info.Mode().Perm()&0o077 != 0 {
		kind := "database file"
		want := os.FileMode(0o600)
		if directory {
			kind = "state directory"
			want = 0o700
		}
		return fmt.Errorf("%w: %s mode %04o is not private (expected no broader than %04o)",
			ErrUnsafeStatePath, kind, info.Mode().Perm(), want)
	}
	return nil
}

func (s *Store) pingWithRetry(ctx context.Context) error {
	var err error
	for attempt := 0; attempt <= s.busyRetries; attempt++ {
		err = s.db.PingContext(ctx)
		if err == nil || !isSQLiteBusy(err) {
			return err
		}
		if attempt == s.busyRetries {
			break
		}
		if err := sleepContext(ctx, retryDelay(attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w: %v", ErrBusy, err)
}

func (s *Store) verifyPragmas(ctx context.Context) error {
	expectedJournalMode := "truncate"
	if s.readOnly {
		// Only WAL mode persists in the database header. A read-only reopen of
		// a rollback-journal database therefore reports SQLite's DELETE default.
		expectedJournalMode = "delete"
	}
	return s.verifyPragmasForJournal(ctx, expectedJournalMode)
}

func (s *Store) verifyPragmasForJournal(ctx context.Context, expectedJournalMode string) error {
	var foreignKeys, busyTimeout, synchronous, recursiveTriggers, trustedSchema, queryOnly int
	var journalMode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		return fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("read journal_mode pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA recursive_triggers`).Scan(&recursiveTriggers); err != nil {
		return fmt.Errorf("read recursive_triggers pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA trusted_schema`).Scan(&trustedSchema); err != nil {
		return fmt.Errorf("read trusted_schema pragma: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
		return fmt.Errorf("read query_only pragma: %w", err)
	}
	wantQueryOnly := 0
	if s.readOnly {
		wantQueryOnly = 1
	}
	if foreignKeys != 1 || busyTimeout <= 0 || !strings.EqualFold(journalMode, expectedJournalMode) || synchronous != 2 ||
		recursiveTriggers != 1 || trustedSchema != 0 || queryOnly != wantQueryOnly {
		return fmt.Errorf("configure sqlite: foreign_keys=%d busy_timeout=%d journal_mode=%s synchronous=%d recursive_triggers=%d trusted_schema=%d query_only=%d",
			foreignKeys, busyTimeout, journalMode, synchronous, recursiveTriggers, trustedSchema, queryOnly)
	}
	return nil
}

type sqlReadWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) initializeSchema(ctx context.Context) error {
	createdAt := formatTime(s.now())
	return s.write(ctx, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		switch version {
		case 0:
			var tableCount int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`,
			).Scan(&tableCount); err != nil {
				return fmt.Errorf("inspect empty schema: %w", err)
			}
			if tableCount != 0 {
				return fmt.Errorf("%w: version 0 database is not empty; legacy migration is unsupported", ErrInvalidSchema)
			}
			for _, statement := range schemaStatements {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("create schema version %d: %w", SchemaVersion, err)
				}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO metadata(key, value) VALUES ('schema_version', ?), ('created_at', ?)`,
				strconv.Itoa(SchemaVersion), createdAt,
			); err != nil {
				return fmt.Errorf("initialize metadata: %w", err)
			}
			if _, err := tx.ExecContext(ctx, schemaVersionSQL); err != nil {
				return fmt.Errorf("set schema version: %w", err)
			}
		case legacySchemaVersion:
			if err := validateSchemaVersion(ctx, tx, legacySchemaVersion,
				schemaStatementsForVersion(legacySchemaVersion)); err != nil {
				return fmt.Errorf("validate schema before migration: %w", err)
			}
			for _, statement := range schemaAdditionsSince(legacySchemaVersion) {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("migrate schema version %d to %d: %w",
						legacySchemaVersion, SchemaVersion, err)
				}
			}
			if _, err := tx.ExecContext(ctx, `DROP TRIGGER metadata_no_update`); err != nil {
				return fmt.Errorf("prepare schema metadata migration: %w", err)
			}
			result, err := tx.ExecContext(ctx,
				`UPDATE metadata SET value = ? WHERE key = 'schema_version'`,
				strconv.Itoa(SchemaVersion),
			)
			if err != nil {
				return fmt.Errorf("update schema metadata: %w", err)
			}
			rows, err := result.RowsAffected()
			if err != nil || rows != 1 {
				return fmt.Errorf("update schema metadata: rows affected=%d, error=%v", rows, err)
			}
			metadataGuard := schemaStatementByName("metadata_no_update")
			if metadataGuard == "" {
				return errors.New("metadata update guard is missing from the current schema")
			}
			if _, err := tx.ExecContext(ctx, metadataGuard); err != nil {
				return fmt.Errorf("restore schema metadata guard: %w", err)
			}
			if _, err := tx.ExecContext(ctx, schemaVersionSQL); err != nil {
				return fmt.Errorf("finish schema migration: %w", err)
			}
		case SchemaVersion:
			if err := validateSchemaVersion(ctx, tx, SchemaVersion, schemaStatements); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: unsupported user_version %d", ErrInvalidSchema, version)
		}
		applicationID, err := readApplicationID(ctx, tx)
		if err != nil {
			return err
		}
		if applicationID != 0 && applicationID != motusApplicationID {
			return fmt.Errorf("%w: unexpected application_id %d", ErrInvalidSchema, applicationID)
		}
		if _, err := tx.ExecContext(ctx, `PRAGMA application_id = `+strconv.FormatUint(motusApplicationID, 10)); err != nil {
			return fmt.Errorf("set Motus application identity: %w", err)
		}
		return validateSchema(ctx, tx)
	})
}

func validateSchema(ctx context.Context, q sqlReadWriter) error {
	structureErr := validateSchemaStructure(ctx, q)
	if structureErr != nil && !errors.Is(structureErr, errSchemaMigrationRequired) {
		return structureErr
	}
	applicationID, err := readApplicationID(ctx, q)
	if err != nil {
		return err
	}
	switch applicationID {
	case motusApplicationID:
	case 0:
		if structureErr == nil {
			return fmt.Errorf("%w: database has no Motus application identity", errIdentityMigrationRequired)
		}
	default:
		return fmt.Errorf("%w: unexpected application_id %d", ErrInvalidSchema, applicationID)
	}
	return structureErr
}

func validateSchemaBeforeJournalMigration(ctx context.Context, q sqlReadWriter) error {
	structureErr := validateSchemaStructure(ctx, q)
	if structureErr != nil && !errors.Is(structureErr, errSchemaMigrationRequired) {
		return structureErr
	}
	applicationID, err := readApplicationID(ctx, q)
	if err != nil {
		return err
	}
	if applicationID != 0 && applicationID != motusApplicationID {
		return fmt.Errorf("%w: unexpected application_id %d", ErrInvalidSchema, applicationID)
	}
	return structureErr
}

func validateSchemaStructure(ctx context.Context, q sqlReadWriter) error {
	var userVersion int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("%w: read user_version: %v", ErrInvalidSchema, err)
	}
	if userVersion == legacySchemaVersion && SchemaVersion != legacySchemaVersion {
		if err := validateSchemaVersion(ctx, q, legacySchemaVersion,
			schemaStatementsForVersion(legacySchemaVersion)); err != nil {
			return err
		}
		return fmt.Errorf("%w: schema version %d", errSchemaMigrationRequired, userVersion)
	}
	return validateSchemaVersion(ctx, q, SchemaVersion, schemaStatements)
}

func readApplicationID(ctx context.Context, q sqlReadWriter) (uint32, error) {
	var applicationID int64
	if err := q.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&applicationID); err != nil {
		return 0, fmt.Errorf("%w: read application_id: %v", ErrInvalidSchema, err)
	}
	if applicationID < 0 || applicationID > int64(^uint32(0)) {
		return 0, fmt.Errorf("%w: application_id %d is out of range", ErrInvalidSchema, applicationID)
	}
	return uint32(applicationID), nil
}

func validateSchemaVersion(
	ctx context.Context,
	q sqlReadWriter,
	expectedVersion int,
	expectedStatements []string,
) error {
	var userVersion int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return fmt.Errorf("%w: read user_version: %v", ErrInvalidSchema, err)
	}
	if userVersion != expectedVersion {
		return fmt.Errorf("%w: unsupported user_version %d", ErrInvalidSchema, userVersion)
	}
	var metadataVersion string
	if err := q.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = 'schema_version'`).Scan(&metadataVersion); err != nil {
		return fmt.Errorf("%w: read metadata schema version: %v", ErrInvalidSchema, err)
	}
	if metadataVersion != strconv.Itoa(expectedVersion) {
		return fmt.Errorf("%w: metadata schema version %q", ErrInvalidSchema, metadataVersion)
	}

	rows, err := q.QueryContext(ctx,
		`SELECT type, name, sql FROM sqlite_schema
		  WHERE type IN ('table', 'index', 'trigger') AND sql IS NOT NULL
		  ORDER BY type, name`,
	)
	if err != nil {
		return fmt.Errorf("%w: inspect schema objects: %v", ErrInvalidSchema, err)
	}
	defer rows.Close()
	definitions := make(map[schemaObjectKey]string)
	for rows.Next() {
		var objectType, name, definition string
		if err := rows.Scan(&objectType, &name, &definition); err != nil {
			return fmt.Errorf("%w: scan schema object: %v", ErrInvalidSchema, err)
		}
		definitions[schemaObjectKey{objectType: objectType, name: name}] = normalizeSchemaSQL(definition)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: inspect schema objects: %v", ErrInvalidSchema, err)
	}
	expectedDefinitions := expectedSchemaDefinitionsFor(expectedStatements)
	var missing []string
	for key := range expectedDefinitions {
		if _, present := definitions[key]; !present {
			missing = append(missing, key.objectType+" "+key.name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return fmt.Errorf("%w: missing required objects: %s", ErrInvalidSchema, strings.Join(missing, ", "))
	}
	for key, expected := range expectedDefinitions {
		if definitions[key] != expected {
			return fmt.Errorf("%w: definition of %s %q differs from schema version %d",
				ErrInvalidSchema, key.objectType, key.name, expectedVersion)
		}
	}
	for key := range definitions {
		if _, expected := expectedDefinitions[key]; !expected {
			return fmt.Errorf("%w: unexpected schema object %s %q", ErrInvalidSchema, key.objectType, key.name)
		}
	}
	return nil
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.readOnly {
		return ErrReadOnly
	}
	var last error
	for attempt := 0; attempt <= s.busyRetries; attempt++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			last = err
		} else {
			err = fn(tx)
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if err == nil {
				return nil
			}
			last = err
		}
		if !isSQLiteBusy(last) {
			return last
		}
		if attempt == s.busyRetries {
			break
		}
		if err := sleepContext(ctx, retryDelay(attempt)); err != nil {
			return err
		}
	}
	return fmt.Errorf("%w after %d retries: %v", ErrBusy, s.busyRetries, last)
}

func isSQLiteBusy(err error) bool {
	return errors.Is(err, sqlite3.BUSY) || errors.Is(err, sqlite3.LOCKED)
}

func retryDelay(attempt int) time.Duration {
	delay := 10 * time.Millisecond * time.Duration(1<<min(attempt, 5))
	return min(delay, 250*time.Millisecond)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
