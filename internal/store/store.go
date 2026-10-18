package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"agentdev/internal/state"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound               = errors.New("任务不存在")
	ErrConcurrentModification = errors.New("任务版本冲突")
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
    id              TEXT PRIMARY KEY,
    repo            TEXT NOT NULL,
    title           TEXT,
    requirement     TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'cli',
    source_ref      TEXT,
    level           TEXT,
    level_override  INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL,
    previous_status TEXT,
    plan_json       TEXT,
    branch          TEXT,
    worktree_path   TEXT,
    base_sha        TEXT,
    commit_sha      TEXT,
    pr_number       INTEGER,
    pr_url          TEXT,
    fix_attempts    INTEGER NOT NULL DEFAULT 0,
    ci_attempts     INTEGER NOT NULL DEFAULT 0,
    review_attempts INTEGER NOT NULL DEFAULT 0,
    budget_usd      REAL NOT NULL,
    spent_usd       REAL NOT NULL DEFAULT 0,
    version         INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    finished_at     TEXT
);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE TABLE IF NOT EXISTS task_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks(id),
    ts          TEXT NOT NULL,
    type        TEXT NOT NULL,
    from_status TEXT,
    to_status   TEXT,
    payload     TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_task ON task_events(task_id, id);
CREATE TABLE IF NOT EXISTS cost_ledger (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id            TEXT NOT NULL REFERENCES tasks(id),
    ts                 TEXT NOT NULL,
    role               TEXT NOT NULL,
    model              TEXT NOT NULL,
    input_tokens       INTEGER NOT NULL,
    output_tokens      INTEGER NOT NULL,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS verification_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id     TEXT NOT NULL REFERENCES tasks(id),
    attempt     INTEGER NOT NULL,
    passed      INTEGER NOT NULL,
    report_json TEXT NOT NULL,
    ts          TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS policy_violations (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ts      TEXT NOT NULL,
    kind    TEXT NOT NULL,
    detail  TEXT NOT NULL,
    blocked INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS interactions (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL REFERENCES tasks(id),
    ts      TEXT NOT NULL,
    kind    TEXT NOT NULL,
    content TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS locks (
    resource     TEXT PRIMARY KEY,
    task_id      TEXT NOT NULL,
    acquired_at  TEXT NOT NULL,
    heartbeat_at TEXT NOT NULL
);
`

// 允许通过 Transition 更新的字段。
var updatableFields = map[string]bool{
	"title": true, "source": true, "source_ref": true, "level": true, "level_override": true,
	"requirement":     true,
	"previous_status": true, "plan_json": true, "branch": true, "worktree_path": true,
	"base_sha": true, "commit_sha": true, "pr_number": true, "pr_url": true,
	"fix_attempts": true, "ci_attempts": true, "review_attempts": true, "budget_usd": true, "spent_usd": true,
	"finished_at": true,
}

type Task struct {
	ID             string
	Repo           string
	Requirement    string
	Title          string
	Source         string
	SourceRef      string
	Level          string
	LevelOverride  bool
	Status         state.Status
	PreviousStatus state.Status
	PlanJSON       string
	Branch         string
	WorktreePath   string
	BaseSHA        string
	CommitSHA      string
	PRNumber       int64
	PRURL          string
	FixAttempts    int
	CIAttempts     int
	ReviewAttempts int
	BudgetUSD      float64
	SpentUSD       float64
	Version        int64
	CreatedAt      string
	UpdatedAt      string
	FinishedAt     string
}

type NewTask struct {
	Repo          string
	Requirement   string
	BudgetUSD     float64
	Level         string
	LevelOverride bool
	Title         string
	Source        string
	SourceRef     string
}

type Event struct {
	ID         int64          `json:"id"`
	TS         string         `json:"ts"`
	Type       string         `json:"type"`
	FromStatus string         `json:"from_status,omitempty"`
	ToStatus   string         `json:"to_status,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
}

type Violation struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
	Blocked bool   `json:"blocked"`
}

type Verification struct {
	Attempt int            `json:"attempt"`
	Passed  bool           `json:"passed"`
	Report  map[string]any `json:"report"`
	TS      string         `json:"ts"`
}

// CostEntry 是一次模型调用的计费记录。
type CostEntry struct {
	TaskID           string  `json:"task_id,omitempty"`
	TS               string  `json:"ts"`
	Role             string  `json:"role"`
	Model            string  `json:"model"`
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

type Interaction struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// Repository 是任务持久化的抽象。当前实现为 SQLite，后续可增加 PostgreSQL 实现。
type Repository interface {
	CreateTask(t NewTask) (Task, error)
	GetTask(id string) (Task, error)
	ListTasks(status state.Status, repo string) ([]Task, error)
	NextQueued() (Task, bool, error)
	NextRunnable() (Task, bool, error)
	Transition(id string, expectedVersion int64, to state.Status, payload map[string]any, fields map[string]any) (Task, error)
	AppendEvent(taskID, eventType string, payload map[string]any) error
	ListEvents(taskID string) ([]Event, error)
	RecordViolation(taskID, kind, detail string, blocked bool) error
	ListViolations(taskID string) ([]Violation, error)
	RecordVerification(taskID string, attempt int, passed bool, report map[string]any) error
	ListVerifications(taskID string) ([]Verification, error)
	RecordCost(taskID, role, model string, entry CostEntry) error
	ListCosts(taskID string) ([]CostEntry, error)
	ListAllCosts() ([]CostEntry, error)
	AddSpend(taskID string, delta float64) error
	RecordInteraction(taskID, kind, content string) error
	ListInteractions(taskID string) ([]Interaction, error)
	Close() error
}

type SQLiteStore struct {
	db *sql.DB
}

func Open(path string) (*SQLiteStore, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate",
		filepath.ToSlash(path),
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}
	if err := migrateColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteStore{db: db}, nil
}

// requiredColumns 是后续版本新增的列。`CREATE TABLE IF NOT EXISTS` 不会给已存在
// 的表补列，因此老库（例如 M1 创建的库）需要显式 ALTER；先查 PRAGMA 再补，
// 重复启动是安全的。
var requiredColumns = []struct {
	name string
	ddl  string
}{
	{"review_attempts", "ALTER TABLE tasks ADD COLUMN review_attempts INTEGER NOT NULL DEFAULT 0"},
}

func migrateColumns(db *sql.DB) error {
	have := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(tasks)")
	if err != nil {
		return fmt.Errorf("读取 tasks 表结构失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("解析 tasks 表结构失败: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range requiredColumns {
		if have[c.name] {
			continue
		}
		if _, err := db.Exec(c.ddl); err != nil {
			return fmt.Errorf("迁移列 %s 失败: %w", c.name, err)
		}
	}
	return nil
}

func (s *SQLiteStore) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func newTaskID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return "t_" + time.Now().UTC().Format("20060102") + "_" + hex.EncodeToString(b)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *SQLiteStore) CreateTask(t NewTask) (Task, error) {
	id := newTaskID()
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO tasks (id, repo, title, requirement, source, source_ref, level, level_override, status,
		 budget_usd, version, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?)`,
		id, t.Repo, nullable(t.Title), t.Requirement, orDefault(t.Source, "cli"),
		nullable(t.SourceRef), nullable(t.Level), boolInt(t.LevelOverride), string(state.Queued),
		t.BudgetUSD, ts, ts,
	); err != nil {
		return Task{}, err
	}
	if err := insertEvent(tx, id, "task_created", "", state.Queued, map[string]any{"repo": t.Repo}, ts); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

const taskColumns = `id, repo, title, requirement, source, source_ref, level, level_override, status,
	previous_status, plan_json, branch, worktree_path, base_sha, commit_sha, pr_number, pr_url,
	fix_attempts, ci_attempts, review_attempts, budget_usd, spent_usd, version, created_at, updated_at, finished_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(r rowScanner) (Task, error) {
	var (
		t                                               Task
		title, sourceRef, level, prev, plan, branch, wt sql.NullString
		base, commit, prURL, finished                   sql.NullString
		prNumber                                        sql.NullInt64
		levelOverride                                   int64
		status                                          string
	)
	err := r.Scan(&t.ID, &t.Repo, &title, &t.Requirement, &t.Source, &sourceRef, &level, &levelOverride,
		&status, &prev, &plan, &branch, &wt, &base, &commit, &prNumber, &prURL,
		&t.FixAttempts, &t.CIAttempts, &t.ReviewAttempts, &t.BudgetUSD, &t.SpentUSD,
		&t.Version, &t.CreatedAt, &t.UpdatedAt, &finished)
	if err != nil {
		return Task{}, err
	}
	t.Title = title.String
	t.SourceRef = sourceRef.String
	t.Level = level.String
	t.LevelOverride = levelOverride != 0
	t.Status = state.Status(status)
	t.PreviousStatus = state.Status(prev.String)
	t.PlanJSON = plan.String
	t.Branch = branch.String
	t.WorktreePath = wt.String
	t.BaseSHA = base.String
	t.CommitSHA = commit.String
	t.PRNumber = prNumber.Int64
	t.PRURL = prURL.String
	t.FinishedAt = finished.String
	return t, nil
}

func (s *SQLiteStore) GetTask(id string) (Task, error) {
	row := s.db.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE id = ?", id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return t, err
}

func (s *SQLiteStore) ListTasks(status state.Status, repo string) ([]Task, error) {
	q := "SELECT " + taskColumns + " FROM tasks WHERE 1=1"
	var args []any
	if status != "" {
		q += " AND status = ?"
		args = append(args, string(status))
	}
	if repo != "" {
		q += " AND repo = ?"
		args = append(args, repo)
	}
	q += " ORDER BY rowid"
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) NextQueued() (Task, bool, error) {
	return s.nextByStatus([]state.Status{state.Queued})
}

// RunnableStatuses 是可以被编排器继续推进的状态集合（含恢复场景）。
var RunnableStatuses = []state.Status{
	state.Queued, state.Triaging, state.Planning, state.Coding, state.Verifying,
	state.Reviewing, state.Committing, state.Pushed, state.PROpened, state.CIWatching,
}

// NextRunnable 取下一个可推进的任务：新任务或上次中断在中间状态的任务。
func (s *SQLiteStore) NextRunnable() (Task, bool, error) {
	return s.nextByStatus(RunnableStatuses)
}

// ListRunnable 一次取出最多 limit 个可推进任务。
//
// 并发执行时先一次性取定任务列表，再分发给 worker：这样同一个任务不会被
// 两个 worker 同时领走（SQLite 没有 SELECT ... FOR UPDATE，逐条领取会重复）。
func (s *SQLiteStore) ListRunnable(limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 1
	}
	placeholders := make([]string, 0, len(RunnableStatuses))
	args := make([]any, 0, len(RunnableStatuses)+1)
	for _, st := range RunnableStatuses {
		placeholders = append(placeholders, "?")
		args = append(args, string(st))
	}
	args = append(args, limit)
	rows, err := s.db.Query("SELECT "+taskColumns+" FROM tasks WHERE status IN ("+
		strings.Join(placeholders, ",")+") ORDER BY rowid LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) nextByStatus(statuses []state.Status) (Task, bool, error) {
	placeholders := make([]string, 0, len(statuses))
	args := make([]any, 0, len(statuses))
	for _, st := range statuses {
		placeholders = append(placeholders, "?")
		args = append(args, string(st))
	}
	row := s.db.QueryRow("SELECT "+taskColumns+" FROM tasks WHERE status IN ("+
		strings.Join(placeholders, ",")+") ORDER BY rowid LIMIT 1", args...)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, err
	}
	return t, true, nil
}

// Transition 在同一事务内完成：版本校验、状态转换合法性校验、字段更新、事件写入。
func (s *SQLiteStore) Transition(
	id string, expectedVersion int64, to state.Status, payload map[string]any, fields map[string]any,
) (Task, error) {
	for k := range fields {
		if !updatableFields[k] {
			return Task{}, fmt.Errorf("不允许更新的字段: %s", k)
		}
	}
	ts := now()
	tx, err := s.db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()

	var current string
	var version int64
	err = tx.QueryRow("SELECT status, version FROM tasks WHERE id = ?", id).Scan(&current, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Task{}, err
	}
	if version != expectedVersion {
		return Task{}, fmt.Errorf("%w: %s 期望 %d，实际 %d", ErrConcurrentModification, id, expectedVersion, version)
	}

	src := state.Status(current)
	assignments := map[string]any{}
	for k, v := range fields {
		assignments[k] = v
	}
	if err := state.AssertTransition(src, to); err != nil {
		return Task{}, err
	}
	assignments["status"] = string(to)
	if state.Suspended[to] {
		// 默认记住挂起前的状态；允许调用方显式指定恢复目标，
		// 例如“验收标准不可执行”应恢复到 PLANNING，而不是再次验证同一份计划。
		if _, explicit := fields["previous_status"]; !explicit {
			assignments["previous_status"] = current
		}
	}
	assignments["version"] = version + 1
	assignments["updated_at"] = ts

	cols := make([]string, 0, len(assignments))
	args := make([]any, 0, len(assignments)+2)
	for k, v := range assignments {
		cols = append(cols, k+" = ?")
		args = append(args, v)
	}
	args = append(args, id, version)
	if _, err := tx.Exec("UPDATE tasks SET "+strings.Join(cols, ", ")+" WHERE id = ? AND version = ?", args...); err != nil {
		return Task{}, err
	}
	if err := insertEvent(tx, id, "state_changed", src, to, payload, ts); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

func insertEvent(tx *sql.Tx, taskID, eventType string, from, to state.Status, payload map[string]any, ts string) error {
	var payloadJSON any
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		payloadJSON = string(b)
	}
	_, err := tx.Exec(
		"INSERT INTO task_events (task_id, ts, type, from_status, to_status, payload) VALUES (?,?,?,?,?,?)",
		taskID, ts, eventType, nullable(string(from)), nullable(string(to)), payloadJSON,
	)
	return err
}

func (s *SQLiteStore) AppendEvent(taskID, eventType string, payload map[string]any) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertEvent(tx, taskID, eventType, "", "", payload, now()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) ListEvents(taskID string) ([]Event, error) {
	rows, err := s.db.Query(
		"SELECT id, ts, type, from_status, to_status, payload FROM task_events WHERE task_id = ? ORDER BY id",
		taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var from, to, payload sql.NullString
		if err := rows.Scan(&e.ID, &e.TS, &e.Type, &from, &to, &payload); err != nil {
			return nil, err
		}
		e.FromStatus = from.String
		e.ToStatus = to.String
		if payload.Valid {
			if err := json.Unmarshal([]byte(payload.String), &e.Payload); err != nil {
				return nil, err
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) RecordViolation(taskID, kind, detail string, blocked bool) error {
	_, err := s.db.Exec(
		"INSERT INTO policy_violations (task_id, ts, kind, detail, blocked) VALUES (?,?,?,?,?)",
		taskID, now(), kind, detail, boolInt(blocked))
	return err
}

func (s *SQLiteStore) ListViolations(taskID string) ([]Violation, error) {
	rows, err := s.db.Query(
		"SELECT ts, kind, detail, blocked FROM policy_violations WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Violation
	for rows.Next() {
		var v Violation
		var blocked int64
		if err := rows.Scan(&v.TS, &v.Kind, &v.Detail, &blocked); err != nil {
			return nil, err
		}
		v.Blocked = blocked != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) RecordVerification(taskID string, attempt int, passed bool, report map[string]any) error {
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		"INSERT INTO verification_runs (task_id, attempt, passed, report_json, ts) VALUES (?,?,?,?,?)",
		taskID, attempt, boolInt(passed), string(b), now())
	return err
}

func (s *SQLiteStore) ListVerifications(taskID string) ([]Verification, error) {
	rows, err := s.db.Query(
		"SELECT attempt, passed, report_json, ts FROM verification_runs WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Verification
	for rows.Next() {
		var v Verification
		var passed int64
		var reportJSON string
		if err := rows.Scan(&v.Attempt, &passed, &reportJSON, &v.TS); err != nil {
			return nil, err
		}
		v.Passed = passed != 0
		if err := json.Unmarshal([]byte(reportJSON), &v.Report); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RecordCost 写入一条计费明细。
func (s *SQLiteStore) RecordCost(taskID, role, model string, entry CostEntry) error {
	_, err := s.db.Exec(
		`INSERT INTO cost_ledger (task_id, ts, role, model, input_tokens, output_tokens,
		 cache_read_tokens, cache_write_tokens, cost_usd) VALUES (?,?,?,?,?,?,?,?,?)`,
		taskID, orNow(entry.TS), role, model, entry.InputTokens, entry.OutputTokens,
		entry.CacheReadTokens, entry.CacheWriteTokens, entry.CostUSD)
	return err
}

func orNow(ts string) string {
	if ts == "" {
		return now()
	}
	return ts
}

func (s *SQLiteStore) ListCosts(taskID string) ([]CostEntry, error) {
	rows, err := s.db.Query(
		`SELECT task_id, ts, role, model, input_tokens, output_tokens, cache_read_tokens,
		 cache_write_tokens, cost_usd FROM cost_ledger WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostEntry
	for rows.Next() {
		var e CostEntry
		if err := rows.Scan(&e.TaskID, &e.TS, &e.Role, &e.Model, &e.InputTokens, &e.OutputTokens,
			&e.CacheReadTokens, &e.CacheWriteTokens, &e.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListAllCosts 返回所有模型调用记录，用于成本看板聚合。
func (s *SQLiteStore) ListAllCosts() ([]CostEntry, error) {
	rows, err := s.db.Query(`SELECT task_id, ts, role, model, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, cost_usd FROM cost_ledger ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostEntry
	for rows.Next() {
		var e CostEntry
		if err := rows.Scan(&e.TaskID, &e.TS, &e.Role, &e.Model, &e.InputTokens, &e.OutputTokens,
			&e.CacheReadTokens, &e.CacheWriteTokens, &e.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AddSpend 累加任务花费。故意不改动 version，避免与正在进行的转换争抢乐观锁。
func (s *SQLiteStore) AddSpend(taskID string, delta float64) error {
	_, err := s.db.Exec(
		"UPDATE tasks SET spent_usd = spent_usd + ?, updated_at = ? WHERE id = ?",
		delta, now(), taskID)
	return err
}

func (s *SQLiteStore) RecordInteraction(taskID, kind, content string) error {
	_, err := s.db.Exec(
		"INSERT INTO interactions (task_id, ts, kind, content) VALUES (?,?,?,?)",
		taskID, now(), kind, content)
	return err
}

func (s *SQLiteStore) ListInteractions(taskID string) ([]Interaction, error) {
	rows, err := s.db.Query(
		"SELECT ts, kind, content FROM interactions WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Interaction
	for rows.Next() {
		var i Interaction
		if err := rows.Scan(&i.TS, &i.Kind, &i.Content); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
