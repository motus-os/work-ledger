package store

import "strings"

const schemaVersionSQL = `PRAGMA user_version = 1`

var schemaStatements = []string{
	`CREATE TABLE metadata (
            key   TEXT PRIMARY KEY,
            value TEXT NOT NULL,
            CHECK (key IN ('schema_version', 'created_at'))
        ) STRICT, WITHOUT ROWID`,
	`CREATE TABLE runs (
            id                  TEXT PRIMARY KEY,
            state               TEXT NOT NULL CHECK (state IN ('open', 'closed')),
            started_at          TEXT NOT NULL,
            ended_at            TEXT,
            created_at          TEXT NOT NULL,
            updated_at          TEXT NOT NULL,
            source              TEXT NOT NULL CHECK (length(source) BETWEEN 1 AND 255),
            producer            TEXT NOT NULL CHECK (length(producer) BETWEEN 1 AND 255),
            executable_basename TEXT NOT NULL CHECK (length(executable_basename) BETWEEN 1 AND 255),
            argument_count      INTEGER NOT NULL CHECK (argument_count >= 0),
            git_repository      TEXT,
            git_commit          TEXT,
	            git_dirty           INTEGER CHECK (git_dirty IS NULL OR git_dirty IN (0, 1)),
            outcome             TEXT CHECK (outcome IS NULL OR outcome IN ('success', 'failure', 'aborted')),
            exit_code           INTEGER,
            signal              TEXT CHECK (signal IS NULL OR length(signal) BETWEEN 1 AND 255),
            timed_out           INTEGER CHECK (timed_out IS NULL OR timed_out IN (0, 1)),
            stdout_bytes        INTEGER NOT NULL CHECK (stdout_bytes >= 0),
            stderr_bytes        INTEGER NOT NULL CHECK (stderr_bytes >= 0),
            stdout_lines        INTEGER NOT NULL CHECK (stdout_lines >= 0),
	            stderr_lines        INTEGER NOT NULL CHECK (stderr_lines >= 0),
	            start_event_id      TEXT NOT NULL,
	            terminal_event_id   TEXT,
            CHECK (length(id) BETWEEN 1 AND 255),
            CHECK (started_at GLOB '????-??-??T??:??:??.?????????Z'),
            CHECK (created_at GLOB '????-??-??T??:??:??.?????????Z'),
            CHECK (updated_at GLOB '????-??-??T??:??:??.?????????Z'),
            CHECK (ended_at IS NULL OR ended_at GLOB '????-??-??T??:??:??.?????????Z'),
            CHECK (updated_at >= created_at),
            CHECK (ended_at IS NULL OR ended_at >= started_at),
            CHECK (exit_code IS NULL OR exit_code >= 0),
            CHECK (stdout_lines <= stdout_bytes),
            CHECK (stderr_lines <= stderr_bytes),
            CHECK (outcome <> 'success' OR
                   (exit_code = 0 AND signal IS NULL AND COALESCE(timed_out, 0) = 0)),
            CHECK (COALESCE(timed_out, 0) = 0 OR outcome = 'aborted'),
            CHECK (
                (state = 'open'
                    AND ended_at IS NULL
                    AND outcome IS NULL
                    AND exit_code IS NULL
                    AND signal IS NULL
                    AND timed_out IS NULL
                    AND stdout_bytes = 0
                    AND stderr_bytes = 0
                    AND stdout_lines = 0
	                    AND stderr_lines = 0
	                    AND start_event_id IS NOT NULL
	                    AND terminal_event_id IS NULL)
                OR
                (state = 'closed'
                    AND ended_at IS NOT NULL
	                    AND outcome IS NOT NULL
	                    AND start_event_id IS NOT NULL
	                    AND terminal_event_id IS NOT NULL)
	            ),
	            FOREIGN KEY (id, start_event_id)
	                REFERENCES events(run_id, id)
	                DEFERRABLE INITIALLY DEFERRED,
	            FOREIGN KEY (id, terminal_event_id)
                REFERENCES events(run_id, id)
                DEFERRABLE INITIALLY DEFERRED
        ) STRICT`,
	`CREATE TABLE events (
            id             TEXT NOT NULL UNIQUE,
            run_id         TEXT NOT NULL,
            sequence       INTEGER NOT NULL CHECK (sequence >= 1),
	            kind           TEXT NOT NULL CHECK (kind IN ('run.started', 'run.terminal')),
            payload        BLOB NOT NULL,
            payload_sha256 BLOB NOT NULL CHECK (length(payload_sha256) = 32),
            recorded_at    TEXT NOT NULL,
            terminal       INTEGER NOT NULL CHECK (terminal IN (0, 1)),
            PRIMARY KEY (run_id, sequence),
            UNIQUE (run_id, id),
            CHECK (length(id) BETWEEN 1 AND 255),
            CHECK (length(payload) <= 1048576),
            CHECK (json_valid(CAST(payload AS TEXT))),
            CHECK (recorded_at GLOB '????-??-??T??:??:??.?????????Z'),
            CHECK ((terminal = 1 AND kind = 'run.terminal') OR
                   (terminal = 0 AND kind <> 'run.terminal')),
            FOREIGN KEY (run_id) REFERENCES runs(id)
        ) STRICT`,
	`CREATE TABLE findings (
            id             TEXT PRIMARY KEY,
            origin_run_id  TEXT NOT NULL,
            payload        BLOB NOT NULL,
            payload_sha256 BLOB NOT NULL CHECK (length(payload_sha256) = 32),
            recorded_at    TEXT NOT NULL,
            CHECK (length(id) BETWEEN 1 AND 255),
            CHECK (length(payload) <= 16384),
            CHECK (json_valid(CAST(payload AS TEXT))),
            CHECK (recorded_at GLOB '????-??-??T??:??:??.?????????Z'),
            FOREIGN KEY (origin_run_id) REFERENCES runs(id)
        ) STRICT`,
	`CREATE TABLE finding_closures (
            id               TEXT PRIMARY KEY,
            finding_id       TEXT NOT NULL UNIQUE,
            disposition      TEXT NOT NULL CHECK (disposition IN ('resolved', 'dismissed')),
            resolving_run_id TEXT,
            payload          BLOB NOT NULL,
            payload_sha256   BLOB NOT NULL CHECK (length(payload_sha256) = 32),
            recorded_at      TEXT NOT NULL,
            CHECK (length(id) BETWEEN 1 AND 255),
            CHECK (length(payload) <= 16384),
            CHECK (json_valid(CAST(payload AS TEXT))),
            CHECK (recorded_at GLOB '????-??-??T??:??:??.?????????Z'),
            CHECK ((disposition = 'resolved' AND resolving_run_id IS NOT NULL)
                OR (disposition = 'dismissed' AND resolving_run_id IS NULL)),
            FOREIGN KEY (finding_id) REFERENCES findings(id),
            FOREIGN KEY (resolving_run_id) REFERENCES runs(id)
        ) STRICT`,
	`CREATE TRIGGER metadata_no_update
        BEFORE UPDATE ON metadata
        BEGIN
            SELECT RAISE(ABORT, 'metadata is immutable');
        END`,
	`CREATE TRIGGER metadata_no_delete
        BEFORE DELETE ON metadata
        BEGIN
            SELECT RAISE(ABORT, 'metadata is immutable');
        END`,
	`CREATE TRIGGER runs_no_delete
        BEFORE DELETE ON runs
        BEGIN
            SELECT RAISE(ABORT, 'runs cannot be deleted');
        END`,
	`CREATE TRIGGER runs_no_identity_mutation
        BEFORE UPDATE ON runs
        WHEN NEW.id IS NOT OLD.id
          OR NEW.started_at IS NOT OLD.started_at
          OR NEW.created_at IS NOT OLD.created_at
          OR NEW.source IS NOT OLD.source
          OR NEW.producer IS NOT OLD.producer
          OR NEW.executable_basename IS NOT OLD.executable_basename
          OR NEW.argument_count IS NOT OLD.argument_count
          OR NEW.git_repository IS NOT OLD.git_repository
          OR NEW.git_commit IS NOT OLD.git_commit
	          OR NEW.git_dirty IS NOT OLD.git_dirty
	          OR NEW.start_event_id IS NOT OLD.start_event_id
        BEGIN
            SELECT RAISE(ABORT, 'run identity and start metadata are immutable');
        END`,
	`CREATE TRIGGER runs_no_terminal_mutation_after_close
        BEFORE UPDATE ON runs
        WHEN OLD.state = 'closed' AND (
             NEW.state IS NOT OLD.state
          OR NEW.ended_at IS NOT OLD.ended_at
          OR NEW.updated_at IS NOT OLD.updated_at
          OR NEW.outcome IS NOT OLD.outcome
          OR NEW.exit_code IS NOT OLD.exit_code
          OR NEW.signal IS NOT OLD.signal
          OR NEW.timed_out IS NOT OLD.timed_out
          OR NEW.stdout_bytes IS NOT OLD.stdout_bytes
          OR NEW.stderr_bytes IS NOT OLD.stderr_bytes
          OR NEW.stdout_lines IS NOT OLD.stdout_lines
          OR NEW.stderr_lines IS NOT OLD.stderr_lines
          OR NEW.terminal_event_id IS NOT OLD.terminal_event_id
        )
        BEGIN
            SELECT RAISE(ABORT, 'closed run terminal data are immutable');
        END`,
	`CREATE TRIGGER runs_only_open_to_closed
        BEFORE UPDATE ON runs
        WHEN NEW.state IS NOT OLD.state
         AND NOT (OLD.state = 'open' AND NEW.state = 'closed')
        BEGIN
            SELECT RAISE(ABORT, 'invalid run state transition');
        END`,
	`CREATE TRIGGER runs_close_requires_terminal_event
        BEFORE UPDATE ON runs
        WHEN OLD.state = 'open' AND NEW.state = 'closed' AND (
            NOT EXISTS (
                SELECT 1 FROM events
                 WHERE run_id = OLD.id
                   AND id = NEW.terminal_event_id
                   AND terminal = 1
                   AND kind = 'run.terminal'
            )
            OR (SELECT sequence FROM events
                 WHERE run_id = OLD.id AND id = NEW.terminal_event_id)
               IS NOT (SELECT MAX(sequence) FROM events WHERE run_id = OLD.id)
        )
        BEGIN
            SELECT RAISE(ABORT, 'close requires the final terminal event');
        END`,
	`CREATE TRIGGER events_no_update
        BEFORE UPDATE ON events
        BEGIN
            SELECT RAISE(ABORT, 'events are append-only');
        END`,
	`CREATE TRIGGER events_no_delete
        BEFORE DELETE ON events
        BEGIN
            SELECT RAISE(ABORT, 'events are append-only');
        END`,
	`CREATE TRIGGER events_no_insert_closed_run
        BEFORE INSERT ON events
        WHEN (SELECT state FROM runs WHERE id = NEW.run_id) = 'closed'
        BEGIN
            SELECT RAISE(ABORT, 'cannot append to a closed run');
        END`,
	`CREATE TRIGGER events_no_insert_after_terminal
        BEFORE INSERT ON events
        WHEN EXISTS (SELECT 1 FROM events WHERE run_id = NEW.run_id AND terminal = 1)
        BEGIN
            SELECT RAISE(ABORT, 'cannot append after a terminal event');
        END`,
	`CREATE TRIGGER events_sequence_contiguous
        BEFORE INSERT ON events
        WHEN NEW.sequence IS NOT COALESCE(
            (SELECT MAX(sequence) + 1 FROM events WHERE run_id = NEW.run_id), 1)
        BEGIN
            SELECT RAISE(ABORT, 'event sequence must be contiguous');
	        END`,
	`CREATE TRIGGER events_lifecycle_shape
	        BEFORE INSERT ON events
	        WHEN (NEW.sequence = 1 AND (NEW.kind <> 'run.started' OR NEW.terminal <> 0))
	          OR (NEW.sequence = 2 AND (NEW.kind <> 'run.terminal' OR NEW.terminal <> 1))
	          OR NEW.sequence > 2
	        BEGIN
	            SELECT RAISE(ABORT, 'event does not fit the version-1 lifecycle');
	        END`,
	`CREATE TRIGGER findings_no_update
        BEFORE UPDATE ON findings
        BEGIN
            SELECT RAISE(ABORT, 'findings are append-only');
        END`,
	`CREATE TRIGGER findings_no_delete
        BEFORE DELETE ON findings
        BEGIN
            SELECT RAISE(ABORT, 'findings are append-only');
        END`,
	`CREATE TRIGGER findings_require_closed_origin
        BEFORE INSERT ON findings
        WHEN NOT EXISTS (
            SELECT 1 FROM runs WHERE id = NEW.origin_run_id AND state = 'closed'
        )
        BEGIN
            SELECT RAISE(ABORT, 'finding origin must be a closed run');
        END`,
	`CREATE TRIGGER finding_closures_no_update
        BEFORE UPDATE ON finding_closures
        BEGIN
            SELECT RAISE(ABORT, 'finding closures are append-only');
        END`,
	`CREATE TRIGGER finding_closures_no_delete
        BEFORE DELETE ON finding_closures
        BEGIN
            SELECT RAISE(ABORT, 'finding closures are append-only');
        END`,
	`CREATE TRIGGER finding_closures_require_successful_resolution
        BEFORE INSERT ON finding_closures
        WHEN NEW.disposition = 'resolved' AND NOT EXISTS (
            SELECT 1 FROM runs
             WHERE id = NEW.resolving_run_id
               AND state = 'closed'
               AND outcome = 'success'
        )
        BEGIN
            SELECT RAISE(ABORT, 'resolved finding requires a closed successful run');
        END`,
}

var requiredTables = []string{"events", "finding_closures", "findings", "metadata", "runs"}

var requiredTriggers = []string{
	"events_no_delete",
	"events_no_insert_after_terminal",
	"events_no_insert_closed_run",
	"events_no_update",
	"events_lifecycle_shape",
	"events_sequence_contiguous",
	"finding_closures_no_delete",
	"finding_closures_no_update",
	"finding_closures_require_successful_resolution",
	"findings_no_delete",
	"findings_no_update",
	"findings_require_closed_origin",
	"metadata_no_delete",
	"metadata_no_update",
	"runs_close_requires_terminal_event",
	"runs_no_delete",
	"runs_no_identity_mutation",
	"runs_no_terminal_mutation_after_close",
	"runs_only_open_to_closed",
}

func expectedSchemaDefinitions() map[string]string {
	expected := make(map[string]string, len(schemaStatements))
	for _, statement := range schemaStatements {
		fields := strings.Fields(statement)
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		case "TABLE", "INDEX", "TRIGGER":
			expected[fields[2]] = normalizeSchemaSQL(statement)
		}
	}
	return expected
}

func normalizeSchemaSQL(statement string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(statement)), " ")
}
