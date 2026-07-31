package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
)

func TestStoreHelperProcess(t *testing.T) {
	if os.Getenv("MOTUS_STORE_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+3 >= len(os.Args) {
		os.Exit(98)
	}
	mode := os.Args[separator+1]
	stateDir := os.Args[separator+2]
	worker := os.Args[separator+3]
	if mode == "invalid-wal-crash" {
		marker := os.Args[separator+4]
		if err := holdInvalidWALStore(stateDir, marker); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(85)
		}
	}
	if mode == "unknown-delete-hot-crash" {
		marker := os.Args[separator+4]
		if err := holdInvalidDeleteStoreWithHotJournal(stateDir, marker, 0); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(84)
		}
	}
	if mode == "identified-invalid-delete-hot-crash" {
		marker := os.Args[separator+4]
		if err := holdInvalidDeleteStoreWithHotJournal(stateDir, marker, motusApplicationID); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(81)
		}
	}
	if mode == "wal-crash" {
		ledger, err := openExistingWALStoreForProcess(context.Background(), stateDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(91)
		}
		runID := "run_wal_crash_" + worker
		_, err = ledger.StartRun(context.Background(), StartRunParams{
			ID: runID, EventID: "event_" + runID + "_started", StartedAt: time.Now().UTC(), Source: "process-test", Producer: "test",
			ExecutableBasename: "helper",
		})
		if err == nil {
			exitCode := 0
			_, err = ledger.CloseRun(context.Background(), runID, CloseRunParams{
				EventID: "terminal_" + runID, EndedAt: time.Now().UTC(), Outcome: OutcomeSuccess, ExitCode: &exitCode,
			})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(90)
		}
		marker := os.Args[separator+4]
		if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(89)
		}
		for {
			runtime.KeepAlive(ledger)
			time.Sleep(time.Hour)
		}
	}
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		writeStoreHelperError(err)
		os.Exit(97)
	}
	if mode == "legacy-delete-hot-crash" {
		var journalMode string
		if err := ledger.db.QueryRow(`PRAGMA journal_mode = DELETE`).Scan(&journalMode); err != nil ||
			!strings.EqualFold(journalMode, "delete") {
			fmt.Fprintf(os.Stderr, "set legacy DELETE journal mode: mode=%q error=%v\n", journalMode, err)
			os.Exit(82)
		}
	}
	if mode == "unmarked-rollback-hot-crash" {
		if _, err := ledger.db.Exec(`PRAGMA application_id = 0`); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(83)
		}
	}
	if mode == "rollback-hot-crash" ||
		mode == "legacy-delete-hot-crash" ||
		mode == "unmarked-rollback-hot-crash" {
		if _, err := ledger.db.Exec(`PRAGMA cache_size = 1`); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(88)
		}
		tx, err := ledger.db.BeginTx(context.Background(), nil)
		if err == nil {
			_, err = tx.Exec(`CREATE TABLE crash_probe(value BLOB)`)
		}
		for index := 0; err == nil && index < 512; index++ {
			_, err = tx.Exec(`INSERT INTO crash_probe(value) VALUES (randomblob(4096))`)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(87)
		}
		marker := os.Args[separator+4]
		if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(86)
		}
		for {
			runtime.KeepAlive(tx)
			runtime.KeepAlive(ledger)
			time.Sleep(time.Hour)
		}
	}
	if mode == "crash" {
		runID := "run_crash_" + worker
		_, err = ledger.StartRun(context.Background(), StartRunParams{
			ID: runID, EventID: "event_" + runID + "_started", StartedAt: time.Now().UTC(), Source: "process-test", Producer: "test",
			ExecutableBasename: "helper",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(96)
		}
		marker := os.Args[separator+4]
		if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(95)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	count, err := strconv.Atoi(os.Args[separator+4])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(94)
	}
	for index := range count {
		runID := fmt.Sprintf("run_process_%s_%03d", worker, index)
		_, err := ledger.StartRun(context.Background(), StartRunParams{
			ID: runID, EventID: "event_" + runID + "_started", StartedAt: time.Now().UTC(), Source: "process-test", Producer: "test",
			ExecutableBasename: "helper",
		})
		if err == nil {
			exitCode := 0
			_, err = ledger.CloseRun(context.Background(), runID, CloseRunParams{
				EventID: "terminal_" + runID, EndedAt: time.Now().UTC(), Outcome: OutcomeSuccess, ExitCode: &exitCode,
			})
		}
		if err != nil {
			writeStoreHelperError(err)
			os.Exit(93)
		}
	}
	if err := ledger.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(92)
	}
	os.Exit(0)
}

func holdInvalidWALStore(stateDir, marker string) error {
	database, err := createPrivateRawDatabase(stateDir)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, "WAL"))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint = 0`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE sentinel(value BLOB)`); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA user_version = 777`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for index := 0; index < 256; index++ {
		if _, err := tx.Exec(`INSERT INTO sentinel(value) VALUES (randomblob(4096))`); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
		return err
	}
	for {
		runtime.KeepAlive(db)
		time.Sleep(time.Hour)
	}
}

func holdInvalidDeleteStoreWithHotJournal(stateDir, marker string, applicationID uint32) error {
	database, err := createPrivateRawDatabase(stateDir)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite3",
		sqliteDSNWithJournalMode(database, false, defaultBusyTimeout, "DELETE"))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE sentinel(value BLOB)`); err != nil {
		return err
	}
	if _, err := db.Exec(`PRAGMA user_version = 777`); err != nil {
		return err
	}
	if applicationID != 0 {
		if _, err := db.Exec(`PRAGMA application_id = ` + strconv.FormatUint(uint64(applicationID), 10)); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`PRAGMA cache_size = 1`); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE crash_probe(value BLOB)`); err != nil {
		return err
	}
	for index := 0; index < 512; index++ {
		if _, err := tx.Exec(`INSERT INTO crash_probe(value) VALUES (randomblob(4096))`); err != nil {
			return err
		}
	}
	if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
		return err
	}
	for {
		runtime.KeepAlive(tx)
		runtime.KeepAlive(db)
		time.Sleep(time.Hour)
	}
}

func createPrivateRawDatabase(stateDir string) (string, error) {
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		return "", err
	}
	database := filepath.Join(stateDir, DatabaseFilename)
	file, err := os.OpenFile(database, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return database, nil
}

func openExistingWALStoreForProcess(ctx context.Context, stateDir string) (*Store, error) {
	dir, database, err := prepareStatePath(stateDir, true)
	if err != nil {
		return nil, err
	}
	u := sqliteFileURL(database, runtime.GOOS == "windows")
	q := u.Query()
	q.Set("mode", "rw")
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(defaultBusyTimeout.Milliseconds(), 10)+")")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "recursive_triggers(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "trusted_schema(OFF)")
	q.Set("_dqs", "false")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		db:          db,
		stateDir:    dir,
		database:    database,
		busyRetries: defaultBusyRetries,
		now: func() time.Time {
			return time.Now().UTC().Round(0)
		},
	}, nil
}

func storeHelperCommand(t *testing.T, arguments ...string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	commandArguments := []string{"-test.run=TestStoreHelperProcess", "--"}
	commandArguments = append(commandArguments, arguments...)
	command := exec.Command(executable, commandArguments...)
	command.Env = append(os.Environ(), "MOTUS_STORE_HELPER=1")
	return command
}

func writeStoreHelperError(err error) {
	fmt.Fprintln(os.Stderr, err)
	var code sqlite3.ExtendedErrorCode
	if errors.As(err, &code) {
		fmt.Fprintf(os.Stderr, "sqlite_code=%d sqlite_extended_code=%d\n", code.Code(), code)
	}
	var detail *sqlite3.Error
	if errors.As(err, &detail) && detail.Unwrap() != nil {
		fmt.Fprintf(os.Stderr, "sqlite_system_error=%v\n", detail.Unwrap())
	}
}

func TestMultipleProcessesWriteWithoutGaps(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	const workers = 4
	const runsPerWorker = 6
	commands := make([]*exec.Cmd, 0, workers)
	outputs := make([]bytes.Buffer, workers)
	for worker := range workers {
		command := storeHelperCommand(t, "write", stateDir, strconv.Itoa(worker), strconv.Itoa(runsPerWorker))
		command.Stdout = &outputs[worker]
		command.Stderr = &outputs[worker]
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, command)
	}
	for index, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("worker %d: %v: %s", index, err, outputs[index].Bytes())
		}
	}
	ledger, err := OpenReadOnly(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	runs, err := ledger.ListRuns(context.Background(), ListRunsOptions{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(runs), workers*runsPerWorker; got != want {
		t.Fatalf("run count = %d, want %d", got, want)
	}
	for _, run := range runs {
		if run.State != RunClosed || run.Outcome != OutcomeSuccess {
			t.Fatalf("run = %#v", run)
		}
	}
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor() = %#v, %v", report, err)
	}
}

func TestAbruptWriterTerminationLeavesConsistentStore(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	marker := filepath.Join(root, "ready")
	command := storeHelperCommand(t, "crash", stateDir, "one", marker)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer ledger.Close()
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor after crash = %#v, %v", report, err)
	}
	run, err := getRun(context.Background(), ledger.db, "run_crash_one")
	if err != nil || run.State != RunOpen {
		t.Fatalf("crash run = %#v, %v", run, err)
	}
}

func TestWALMigrationRecoversCommittedDataAfterAbruptTermination(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	marker := filepath.Join(root, "wal-ready")
	ledger, err := Open(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Close(); err != nil {
		t.Fatal(err)
	}
	setJournalModeForTest(t, database, "WAL")

	command := storeHelperCommand(t, "wal-crash", stateDir, "one", marker)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatalf("WAL helper did not become ready: %s", output.Bytes())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(database + "-wal"); err != nil {
		_ = command.Process.Kill()
		t.Fatalf("WAL sidecar before crash: %v", err)
	}
	if _, err := OpenForRead(context.Background(), stateDir); !errors.Is(err, errJournalMigrationRequired) ||
		!strings.Contains(err.Error(), "stop every Motus process") {
		_ = command.Process.Kill()
		t.Fatalf("OpenForRead while an older WAL writer is active = %v", err)
	}
	if _, err := Open(context.Background(), stateDir); !errors.Is(err, errJournalMigrationRequired) ||
		!strings.Contains(err.Error(), "stop every Motus process") {
		_ = command.Process.Kill()
		t.Fatalf("Open while an older WAL writer is active = %v", err)
	}
	if requiresMigration, err := requiresJournalMigration(database); err != nil || !requiresMigration {
		_ = command.Process.Kill()
		t.Fatalf("active WAL migration changed journal mode: requires=%t err=%v", requiresMigration, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	readOnly, err := OpenForRead(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("OpenForRead after WAL crash: %v", err)
	}
	defer readOnly.Close()
	run, err := readOnly.GetRun(context.Background(), "run_wal_crash_one")
	if err != nil || run.State != RunClosed || run.Outcome != OutcomeSuccess {
		t.Fatalf("recovered WAL run = %#v, %v", run, err)
	}
	report, err := readOnly.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor after WAL recovery = %#v, %v", report, err)
	}
	var journalMode string
	if err := readOnly.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil ||
		journalMode != "delete" {
		t.Fatalf("journal mode after WAL recovery = %q, %v", journalMode, err)
	}
}

func TestOpenForReadRecoversHotRollbackJournal(t *testing.T) {
	for _, mode := range []string{"rollback-hot-crash", "legacy-delete-hot-crash"} {
		t.Run(mode, func(t *testing.T) {
			testOpenForReadRecoversHotRollbackJournal(t, mode)
		})
	}
}

func testOpenForReadRecoversHotRollbackJournal(t *testing.T, mode string) {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	marker := filepath.Join(root, "rollback-hot-ready")
	command := storeHelperCommand(t, mode, stateDir, "one", marker)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatalf("rollback helper did not become ready: %s", output.Bytes())
		}
		time.Sleep(10 * time.Millisecond)
	}
	journal := database + "-journal"
	if info, err := os.Stat(journal); err != nil || info.Size() == 0 {
		_ = command.Process.Kill()
		t.Fatalf("rollback journal before crash = %#v, %v", info, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	if _, err := OpenReadOnly(context.Background(), stateDir); !errors.Is(err, sqlite3.READONLY_ROLLBACK) {
		t.Fatalf("OpenReadOnly(hot journal) error = %v, want SQLITE_READONLY_ROLLBACK", err)
	}
	recognized, err := hasMotusApplicationID(database)
	if err != nil || !recognized {
		t.Fatalf("hot ledger identity = %t, %v", recognized, err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(database, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(journal, 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(stateDir, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenForRead(context.Background(), stateDir); !errors.Is(err, errRollbackRecoveryRequired) ||
			!strings.Contains(err.Error(), "make the directory and its files writable") {
			t.Fatalf("OpenForRead(non-writable hot journal) error = %v", err)
		}
		if err := os.Chmod(stateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(database, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(journal, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ledger, err := OpenForRead(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("OpenForRead(hot journal) = %v", err)
	}
	defer ledger.Close()
	report, err := ledger.Doctor(context.Background())
	if err != nil || !report.Consistent {
		t.Fatalf("Doctor after rollback recovery = %#v, %v", report, err)
	}
	var crashProbe int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name = 'crash_probe'`).Scan(&crashProbe); err != nil ||
		crashProbe != 0 {
		t.Fatalf("uncommitted crash table survived recovery = %d, %v", crashProbe, err)
	}
	if hot, err := hasRollbackJournal(database); err != nil || hot {
		t.Fatalf("hot rollback journal after recovery = %t, %v", hot, err)
	}
}

type capturedStateFile struct {
	contents []byte
	mode     os.FileMode
	size     int64
	modTime  time.Time
}

type capturedStateDirectory struct {
	info  os.FileInfo
	files map[string]capturedStateFile
}

func captureStateDirectory(t *testing.T, stateDir string) capturedStateDirectory {
	t.Helper()
	directoryInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	captured := capturedStateDirectory{
		info:  directoryInfo,
		files: make(map[string]capturedStateFile, len(entries)),
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			t.Fatalf("unexpected state entry %q with type %v", entry.Name(), entry.Type())
		}
		path := filepath.Join(stateDir, entry.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		captured.files[entry.Name()] = capturedStateFile{
			contents: contents,
			mode:     info.Mode(),
			size:     info.Size(),
			modTime:  info.ModTime(),
		}
	}
	return captured
}

func assertStateDirectoryUnchanged(
	t *testing.T,
	stateDir string,
	before capturedStateDirectory,
) {
	t.Helper()
	after := captureStateDirectory(t, stateDir)
	if !os.SameFile(before.info, after.info) ||
		before.info.Mode() != after.info.Mode() ||
		before.info.Size() != after.info.Size() ||
		!before.info.ModTime().Equal(after.info.ModTime()) {
		t.Fatal("state directory metadata changed during rejected open")
	}
	if len(after.files) != len(before.files) {
		t.Fatalf("state entry count = %d, want %d; after=%v before=%v",
			len(after.files), len(before.files), mapKeys(after.files), mapKeys(before.files))
	}
	for name, want := range before.files {
		got, exists := after.files[name]
		if !exists {
			t.Fatalf("state entry %q disappeared; after=%v", name, mapKeys(after.files))
		}
		if !bytes.Equal(got.contents, want.contents) ||
			got.mode != want.mode ||
			got.size != want.size ||
			!got.modTime.Equal(want.modTime) {
			t.Fatalf("state entry %q changed during rejected open", name)
		}
	}
}

func mapKeys(values map[string]capturedStateFile) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func crashStoreHelper(t *testing.T, mode, stateDir, marker string) {
	t.Helper()
	command := storeHelperCommand(t, mode, stateDir, "one", marker)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("%s helper did not become ready: %s", mode, output.Bytes())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
}

func TestInvalidUncheckpointedWALIsRejectedWithoutSourceMutation(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	database := filepath.Join(stateDir, DatabaseFilename)
	crashStoreHelper(t, "invalid-wal-crash", stateDir, filepath.Join(root, "ready"))
	if info, err := os.Stat(database + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("invalid WAL sidecar = %#v, %v", info, err)
	}
	if required, err := requiresJournalMigration(database); err != nil || !required {
		t.Fatalf("invalid WAL requires migration = %t, %v", required, err)
	}
	before := captureStateDirectory(t, stateDir)

	openers := []struct {
		name string
		open func(context.Context, string) (*Store, error)
	}{
		{name: "OpenForRead", open: OpenForRead},
		{name: "Open", open: Open},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			ledger, err := opener.open(context.Background(), stateDir)
			if ledger != nil {
				_ = ledger.Close()
			}
			if !errors.Is(err, ErrInvalidSchema) {
				t.Fatalf("%s(invalid uncheckpointed WAL) error = %v, want ErrInvalidSchema",
					opener.name, err)
			}
			assertStateDirectoryUnchanged(t, stateDir, before)
		})
	}
}

func TestInvalidHotRollbackJournalIsRejectedWithoutSourceMutation(t *testing.T) {
	cases := []struct {
		name          string
		helperMode    string
		applicationID uint32
	}{
		{name: "unknown", helperMode: "unknown-delete-hot-crash"},
		{
			name:          "Motus-identified",
			helperMode:    "identified-invalid-delete-hot-crash",
			applicationID: motusApplicationID,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			database := filepath.Join(stateDir, DatabaseFilename)
			journal := database + "-journal"
			crashStoreHelper(t, testCase.helperMode, stateDir, filepath.Join(root, "ready"))
			if info, err := os.Stat(journal); err != nil || info.Size() == 0 {
				t.Fatalf("invalid hot rollback journal = %#v, %v", info, err)
			}
			header, err := inspectSQLiteHeader(database)
			if err != nil || !header.valid || header.applicationID != testCase.applicationID {
				t.Fatalf("invalid hot database header = %#v, %v; want application_id=%d",
					header, err, testCase.applicationID)
			}
			before := captureStateDirectory(t, stateDir)

			openers := []struct {
				name string
				open func(context.Context, string) (*Store, error)
			}{
				{name: "OpenForRead", open: OpenForRead},
				{name: "Open", open: Open},
			}
			for _, opener := range openers {
				t.Run(opener.name, func(t *testing.T) {
					ledger, err := opener.open(context.Background(), stateDir)
					if ledger != nil {
						_ = ledger.Close()
					}
					if !errors.Is(err, ErrInvalidSchema) {
						t.Fatalf("%s(invalid hot rollback journal) error = %v, want ErrInvalidSchema",
							opener.name, err)
					}
					assertStateDirectoryUnchanged(t, stateDir, before)
				})
			}
		})
	}
}

func TestUnmarkedHotRollbackJournalRecoversAfterIsolatedValidation(t *testing.T) {
	openers := []struct {
		name string
		open func(context.Context, string) (*Store, error)
	}{
		{name: "OpenForRead", open: OpenForRead},
		{name: "Open", open: Open},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			database := filepath.Join(stateDir, DatabaseFilename)
			journal := database + "-journal"
			crashStoreHelper(t, "unmarked-rollback-hot-crash", stateDir, filepath.Join(root, "ready"))
			header, err := inspectSQLiteHeader(database)
			if err != nil || !header.valid || header.applicationID != 0 {
				t.Fatalf("unmarked hot database header = %#v, %v", header, err)
			}
			if info, err := os.Stat(journal); err != nil || info.Size() == 0 {
				t.Fatalf("unmarked hot rollback journal = %#v, %v", info, err)
			}

			ledger, err := opener.open(context.Background(), stateDir)
			if err != nil {
				t.Fatalf("%s(unmarked hot rollback journal) = %v", opener.name, err)
			}
			report, err := ledger.Doctor(context.Background())
			if err != nil || !report.Consistent {
				_ = ledger.Close()
				t.Fatalf("Doctor after unmarked rollback recovery = %#v, %v", report, err)
			}
			if err := ledger.Close(); err != nil {
				t.Fatal(err)
			}
			recognized, err := hasMotusApplicationID(database)
			if err != nil || !recognized {
				t.Fatalf("recovered ledger identity = %t, %v", recognized, err)
			}
			if hot, err := hasRollbackJournal(database); err != nil || hot {
				t.Fatalf("hot rollback journal after recovery = %t, %v", hot, err)
			}
		})
	}
}
