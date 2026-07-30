package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var errLedgerChangedDuringValidation = errors.New("store: ledger changed during validation")

const rollbackValidationRetries = 20

type validationFile struct {
	source      string
	destination string
	exists      bool
	info        os.FileInfo
	digest      [sha256.Size]byte
}

func hasRollbackJournal(database string) (bool, error) {
	info, exists, err := inspectRegularStateFile(database+"-journal", "rollback journal")
	if err != nil || !exists {
		return false, err
	}
	// TRUNCATE mode leaves a safe, zero-length journal after a clean commit.
	// A non-empty journal may be hot and requires the recovery path.
	return info.Size() > 0, nil
}

func inspectRegularStateFile(path, kind string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect %s %q: %w", kind, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%w: %s %q is not a regular file", ErrUnsafeStatePath, kind, path)
	}
	return info, true, nil
}

func copyValidationFile(source, destination, kind string, required bool) (validationFile, error) {
	snapshot := validationFile{source: source, destination: destination}
	info, exists, err := inspectRegularStateFile(source, kind)
	if err != nil {
		return snapshot, err
	}
	if !exists {
		if required {
			return snapshot, fmt.Errorf("%w: required %s %q disappeared; stop every Motus process using this state directory and retry",
				errLedgerChangedDuringValidation,
				kind, source)
		}
		return snapshot, nil
	}

	sourceFile, err := os.Open(source)
	if err != nil {
		return snapshot, fmt.Errorf("open %s %q for validation: %w", kind, source, err)
	}
	openedInfo, statErr := sourceFile.Stat()
	if statErr != nil {
		_ = sourceFile.Close()
		return snapshot, fmt.Errorf("reinspect open %s %q: %w", kind, source, statErr)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = sourceFile.Close()
		return snapshot, fmt.Errorf("%w: %s %q changed while opening it", ErrUnsafeStatePath, kind, source)
	}

	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = sourceFile.Close()
		return snapshot, fmt.Errorf("create private validation copy for %s: %w", kind, err)
	}
	hasher := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(destinationFile, hasher), sourceFile)
	destinationCloseErr := destinationFile.Close()
	sourceCloseErr := sourceFile.Close()
	if copyErr != nil {
		return snapshot, fmt.Errorf("copy %s for isolated validation: %w", kind, copyErr)
	}
	if destinationCloseErr != nil {
		return snapshot, fmt.Errorf("close validation copy for %s: %w", kind, destinationCloseErr)
	}
	if sourceCloseErr != nil {
		return snapshot, fmt.Errorf("close source %s after validation copy: %w", kind, sourceCloseErr)
	}
	if copied != info.Size() {
		return snapshot, fmt.Errorf("%w: %s %q changed size during copy; stop every Motus process using this state directory and retry",
			errLedgerChangedDuringValidation,
			kind, source)
	}

	snapshot.exists = true
	snapshot.info = info
	copy(snapshot.digest[:], hasher.Sum(nil))
	return snapshot, nil
}

func (snapshot validationFile) verify(kind string) error {
	info, exists, err := inspectRegularStateFile(snapshot.source, kind)
	if err != nil {
		return err
	}
	if exists != snapshot.exists {
		return fmt.Errorf("%w: %s %q appeared or disappeared; stop every Motus process using this state directory and retry",
			errLedgerChangedDuringValidation,
			kind, snapshot.source)
	}
	if !exists {
		return nil
	}
	if !os.SameFile(snapshot.info, info) || info.Size() != snapshot.info.Size() {
		return fmt.Errorf("%w: %s %q was replaced or resized; stop every Motus process using this state directory and retry",
			errLedgerChangedDuringValidation,
			kind, snapshot.source)
	}

	file, err := os.Open(snapshot.source)
	if err != nil {
		return fmt.Errorf("reopen %s %q after validation: %w", kind, snapshot.source, err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("reinspect open %s %q after validation: %w", kind, snapshot.source, statErr)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("%w: %s %q changed while reopening it", ErrUnsafeStatePath, kind, snapshot.source)
	}
	hasher := sha256.New()
	read, hashErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if hashErr != nil {
		return fmt.Errorf("rehash %s after validation: %w", kind, hashErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s after validation hash: %w", kind, closeErr)
	}
	if read != snapshot.info.Size() || !equalDigest(snapshot.digest, hasher.Sum(nil)) {
		return fmt.Errorf("%w: %s %q contents changed; stop every Motus process using this state directory and retry",
			errLedgerChangedDuringValidation,
			kind, snapshot.source)
	}
	return nil
}

func equalDigest(expected [sha256.Size]byte, actual []byte) bool {
	if len(actual) != len(expected) {
		return false
	}
	var difference byte
	for index := range expected {
		difference |= expected[index] ^ actual[index]
	}
	return difference == 0
}

func validateWALSnapshot(ctx context.Context, stateDir, database string) error {
	if _, _, err := inspectRegularStateFile(database+"-shm", "WAL shared-memory sidecar"); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt <= defaultBusyRetries; attempt++ {
		err = validateLedgerSnapshot(ctx, stateDir, database, "-wal", "WAL sidecar", false,
			map[string]bool{"wal": true, "delete": true})
		if !errors.Is(err, errLedgerChangedDuringValidation) {
			return err
		}
		if attempt == defaultBusyRetries {
			break
		}
		if sleepErr := sleepContext(ctx, retryDelay(attempt)); sleepErr != nil {
			return sleepErr
		}
	}
	return fmt.Errorf("%w: %v", ErrBusy, err)
}

func validateRollbackSnapshot(ctx context.Context, stateDir, database string) error {
	var err error
	for attempt := 0; attempt <= rollbackValidationRetries; attempt++ {
		header, headerErr := inspectSQLiteHeader(database)
		if headerErr != nil {
			return headerErr
		}
		if header.valid && header.applicationID == motusApplicationID {
			return nil
		}
		info, exists, inspectErr := inspectRegularStateFile(database+"-journal", "rollback journal")
		if inspectErr != nil {
			return inspectErr
		}
		if !exists || info.Size() == 0 {
			return nil
		}
		err = validateLedgerSnapshot(ctx, stateDir, database, "-journal", "rollback journal", true,
			map[string]bool{"delete": true})
		if !errors.Is(err, errLedgerChangedDuringValidation) {
			return err
		}
		if attempt == rollbackValidationRetries {
			break
		}
		if sleepErr := sleepContext(ctx, retryDelay(attempt)); sleepErr != nil {
			return sleepErr
		}
	}
	return fmt.Errorf("%w: %v", ErrBusy, err)
}

func validateLedgerSnapshot(
	ctx context.Context,
	stateDir, database, sidecarSuffix, sidecarKind string,
	requireSidecar bool,
	allowedJournalModes map[string]bool,
) (returnErr error) {
	validationDir, err := createValidationDirectory(stateDir)
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(validationDir); cleanupErr != nil {
			returnErr = errors.Join(returnErr,
				fmt.Errorf("remove isolated ledger validation directory: %w", cleanupErr))
		}
	}()

	databaseCopy := filepath.Join(validationDir, DatabaseFilename)
	databaseSnapshot, err := copyValidationFile(database, databaseCopy, "database", true)
	if err != nil {
		return err
	}
	sidecarSnapshot, err := copyValidationFile(
		database+sidecarSuffix,
		databaseCopy+sidecarSuffix,
		sidecarKind,
		requireSidecar,
	)
	if err != nil {
		return err
	}
	validationErr := validateCopiedLedger(ctx, databaseCopy, allowedJournalModes)
	databaseVerifyErr := databaseSnapshot.verify("database")
	sidecarVerifyErr := sidecarSnapshot.verify(sidecarKind)
	if databaseVerifyErr != nil || sidecarVerifyErr != nil {
		return errors.Join(databaseVerifyErr, sidecarVerifyErr)
	}
	if validationErr != nil {
		return validationErr
	}
	return nil
}

func createValidationDirectory(stateDir string) (string, error) {
	resolvedState, err := filepath.Abs(filepath.Clean(stateDir))
	if err != nil {
		return "", fmt.Errorf("resolve state directory for isolated ledger validation: %w", err)
	}
	resolvedState, err = filepath.EvalSymlinks(resolvedState)
	if err != nil {
		return "", fmt.Errorf("resolve state directory for isolated ledger validation: %w", err)
	}
	base, err := filepath.Abs(filepath.Clean(os.TempDir()))
	if err != nil {
		return "", fmt.Errorf("resolve temporary directory for isolated ledger validation: %w", err)
	}
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve temporary directory for isolated ledger validation: %w", err)
	}
	insideState, err := pathWithin(base, resolvedState)
	if err != nil {
		return "", err
	}
	if insideState {
		base = filepath.Dir(resolvedState)
	}

	validationDir, err := os.MkdirTemp(base, "motus-ledger-validation-")
	if err != nil {
		return "", fmt.Errorf("create private isolated ledger validation directory: %w", err)
	}
	info, err := os.Lstat(validationDir)
	if err != nil {
		_ = os.RemoveAll(validationDir)
		return "", fmt.Errorf("inspect isolated ledger validation directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		_ = os.RemoveAll(validationDir)
		return "", fmt.Errorf("%w: isolated ledger validation path is not a directory", ErrUnsafeStatePath)
	}
	if err := requirePrivateMode(info, true); err != nil {
		_ = os.RemoveAll(validationDir)
		return "", err
	}
	return validationDir, nil
}

func pathWithin(candidate, root string) (bool, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, fmt.Errorf("compare isolated validation and state paths: %w", err)
	}
	return relative == "." ||
		(relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func validateCopiedLedger(
	ctx context.Context,
	database string,
	allowedJournalModes map[string]bool,
) (returnErr error) {
	db, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, ""))
	if err != nil {
		return fmt.Errorf("open isolated ledger validation copy: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr,
				fmt.Errorf("close isolated ledger validation copy: %w", closeErr))
		}
	}()

	ledger := &Store{
		db:          db,
		database:    database,
		busyRetries: defaultBusyRetries,
		now: func() time.Time {
			return time.Now().UTC().Round(0)
		},
	}
	if err := ledger.pingWithRetry(ctx); err != nil {
		return fmt.Errorf("%w: recover isolated ledger validation copy: %v", ErrInvalidSchema, err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fmt.Errorf("%w: read isolated validation journal mode: %v", ErrInvalidSchema, err)
	}
	journalMode = strings.ToLower(journalMode)
	if !allowedJournalModes[journalMode] {
		return fmt.Errorf("%w: isolated validation copy has unexpected journal mode %q",
			ErrInvalidSchema, journalMode)
	}
	if err := ledger.verifyPragmasForJournal(ctx, journalMode); err != nil {
		return fmt.Errorf("validate isolated ledger configuration: %w", err)
	}
	if err := validateSchemaForRecovery(ctx, db); err != nil {
		return fmt.Errorf("validate isolated recovered ledger: %w", err)
	}
	return nil
}

func validateSchemaForRecovery(ctx context.Context, q sqlReadWriter) error {
	err := validateSchemaBeforeJournalMigration(ctx, q)
	if err == nil || errors.Is(err, errSchemaMigrationRequired) {
		return nil
	}
	empty, emptyErr := isEmptyDatabase(ctx, q)
	if emptyErr != nil {
		return errors.Join(err, emptyErr)
	}
	if empty {
		return nil
	}
	return err
}

func isEmptyDatabase(ctx context.Context, q sqlReadWriter) (bool, error) {
	var userVersion int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return false, fmt.Errorf("%w: inspect empty database user_version: %v", ErrInvalidSchema, err)
	}
	applicationID, err := readApplicationID(ctx, q)
	if err != nil {
		return false, err
	}
	var tableCount int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&tableCount); err != nil {
		return false, fmt.Errorf("%w: inspect empty database schema: %v", ErrInvalidSchema, err)
	}
	return userVersion == 0 && tableCount == 0 &&
		(applicationID == 0 || applicationID == motusApplicationID), nil
}

// migrateWALJournal validates an isolated copy before opening the source
// writable. The Motus identity is committed while the source is still in WAL,
// so an interruption cannot leave an unmarked rollback-journal database.
func migrateWALJournal(ctx context.Context, stateDir, database string) (returnErr error) {
	if err := validateWALSnapshot(ctx, stateDir, database); err != nil {
		return fmt.Errorf("validate WAL ledger before journal migration: %w", err)
	}
	if err := verifyStatePath(stateDir, database); err != nil {
		return err
	}

	db, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, ""))
	if err != nil {
		return fmt.Errorf("open validated WAL ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(0)
	db.SetConnMaxLifetime(0)
	closed := false
	defer func() {
		if !closed {
			if closeErr := db.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close validated WAL ledger: %w", closeErr))
			}
		}
	}()

	ledger := &Store{
		db:          db,
		stateDir:    stateDir,
		database:    database,
		busyRetries: defaultBusyRetries,
		now: func() time.Time {
			return time.Now().UTC().Round(0)
		},
	}
	if err := ledger.pingWithRetry(ctx); err != nil {
		return fmt.Errorf("open validated WAL ledger: %w", err)
	}
	if err := verifyStatePath(stateDir, database); err != nil {
		return err
	}
	var currentJournalMode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&currentJournalMode); err != nil {
		return fmt.Errorf("read validated WAL ledger journal mode: %w", err)
	}
	currentJournalMode = strings.ToLower(currentJournalMode)
	if currentJournalMode != "wal" && currentJournalMode != "delete" {
		return fmt.Errorf("validate WAL ledger configuration: unexpected journal mode %q", currentJournalMode)
	}
	if err := ledger.verifyPragmasForJournal(ctx, currentJournalMode); err != nil {
		return fmt.Errorf("validate WAL ledger configuration: %w", err)
	}
	if err := validateSchemaForRecovery(ctx, db); err != nil {
		return fmt.Errorf("revalidate WAL ledger before journal migration: %w", err)
	}

	if err := ensureMotusIdentity(ctx, db); err != nil {
		return err
	}

	if currentJournalMode == "wal" {
		if err := checkpointMotusIdentity(ctx, db, database); err != nil {
			return err
		}

		var journalMode string
		if err := db.QueryRowContext(ctx, `PRAGMA journal_mode = TRUNCATE`).Scan(&journalMode); err != nil {
			return fmt.Errorf("change validated ledger from WAL to rollback journal: %w", err)
		}
		if !strings.EqualFold(journalMode, "truncate") {
			return fmt.Errorf("change validated ledger from WAL to rollback journal: SQLite retained %q; stop every Motus process using this state directory and retry",
				journalMode)
		}
	}
	if err := db.Close(); err != nil {
		closed = true
		return fmt.Errorf("close migrated WAL ledger: %w", err)
	}
	closed = true

	header, err := inspectSQLiteHeader(database)
	if err != nil {
		return err
	}
	if !header.valid || header.writeVersion != 1 || header.readVersion != 1 ||
		header.applicationID != motusApplicationID {
		return fmt.Errorf("verify migrated ledger header: write_version=%d read_version=%d application_id=%d",
			header.writeVersion, header.readVersion, header.applicationID)
	}
	return nil
}

func ensureMotusIdentity(ctx context.Context, q sqlReadWriter) error {
	applicationID, err := readApplicationID(ctx, q)
	if err != nil {
		return err
	}
	if applicationID == 0 {
		if _, err := q.ExecContext(ctx,
			`PRAGMA application_id = `+strconv.FormatUint(motusApplicationID, 10)); err != nil {
			return fmt.Errorf("commit Motus identity before journal migration: %w", err)
		}
	}
	applicationID, err = readApplicationID(ctx, q)
	if err != nil {
		return err
	}
	if applicationID != motusApplicationID {
		return fmt.Errorf("%w: validated ledger retained application_id %d",
			ErrInvalidSchema, applicationID)
	}
	return nil
}

func checkpointMotusIdentity(ctx context.Context, db *sql.DB, database string) error {
	var busy, logFrames, checkpointedFrames int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(FULL)`).Scan(
		&busy, &logFrames, &checkpointedFrames,
	); err != nil {
		return fmt.Errorf("checkpoint Motus identity before journal migration: %w", err)
	}
	if busy != 0 || checkpointedFrames != logFrames {
		return fmt.Errorf("checkpoint Motus identity before journal migration: busy=%d log_frames=%d checkpointed_frames=%d; stop every Motus process using this state directory and retry",
			busy, logFrames, checkpointedFrames)
	}
	header, err := inspectSQLiteHeader(database)
	if err != nil {
		return err
	}
	if !header.valid || header.applicationID != motusApplicationID {
		return fmt.Errorf("verify Motus identity before journal migration: application_id=%d",
			header.applicationID)
	}
	return nil
}
