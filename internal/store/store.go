package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusReceived     = "received"
	StatusQueued       = "queued"
	StatusRunning      = "running"
	StatusWaitingUser  = "waiting_user"
	StatusResuming     = "resuming"
	StatusCompleted    = "completed"
	StatusFailed       = "failed"
	StatusCancelled    = "cancelled"
	StatusResumeFailed = "resume_failed"
)

type Store struct {
	db   *sql.DB
	path string
}

type Task struct {
	ID, SourceMessageID, Channel, Sender, Conversation string
	Agent, Project, ProjectPath, Prompt, Status        string
	AgentSessionID, PendingQuestion, Result, Error     string
	CreatedAt, UpdatedAt                               time.Time
}

type Message struct {
	ID                                           int64
	TaskID, Direction, Kind, Content, ExternalID string
	CreatedAt                                    time.Time
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 状态库: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	statements := []string{
		`PRAGMA journal_mode=WAL`, `PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS inbound_messages (
			channel TEXT NOT NULL, message_id TEXT NOT NULL, sender TEXT NOT NULL,
			body TEXT NOT NULL, disposition TEXT NOT NULL DEFAULT 'received', received_at TEXT NOT NULL,
			PRIMARY KEY(channel, message_id))`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY, source_message_id TEXT NOT NULL, channel TEXT NOT NULL,
			sender TEXT NOT NULL, conversation TEXT NOT NULL, agent TEXT NOT NULL, project TEXT NOT NULL,
			project_path TEXT NOT NULL, prompt TEXT NOT NULL, status TEXT NOT NULL,
			agent_session_id TEXT NOT NULL DEFAULT '', pending_question TEXT NOT NULL DEFAULT '',
			result TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(channel, source_message_id))`,
		`CREATE INDEX IF NOT EXISTS tasks_sender_status ON tasks(sender, status, updated_at)`,
		`CREATE TABLE IF NOT EXISTS task_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL,
			direction TEXT NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
			external_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL,
			FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE)`,
		`CREATE TABLE IF NOT EXISTS channel_state (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("初始化 SQLite: %w", err)
		}
	}
	return nil
}

func (s *Store) RecoverInterrupted() error {
	// A process cannot safely reattach to a child that was running before a
	// crash. Preserve the session id and make the interruption explicit.
	_, err := s.db.Exec(`UPDATE tasks SET status=?, error=?, updated_at=? WHERE status IN (?,?)`,
		StatusResumeFailed, "Taskian 重启中断了 Agent 进程；请创建新任务或人工检查原会话", nowText(), StatusRunning, StatusResuming)
	return err
}

func (s *Store) ClaimInbound(channel, id, sender, body string, at time.Time) (bool, error) {
	result, err := s.db.Exec(`INSERT OR IGNORE INTO inbound_messages(channel,message_id,sender,body,received_at) VALUES(?,?,?,?,?)`, channel, id, sender, body, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) MarkInbound(channel, id, disposition string) error {
	_, err := s.db.Exec(`UPDATE inbound_messages SET disposition=? WHERE channel=? AND message_id=?`, disposition, channel, id)
	return err
}

func (s *Store) CreateTask(t Task) (Task, error) {
	if t.ID == "" {
		t.ID = newTaskID()
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	if t.Status == "" {
		t.Status = StatusQueued
	}
	_, err := s.db.Exec(`INSERT INTO tasks(id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SourceMessageID, t.Channel, t.Sender, t.Conversation, t.Agent, t.Project, t.ProjectPath, t.Prompt, t.Status, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return Task{}, err
	}
	_ = s.AddMessage(t.ID, "in", "task", t.Prompt, t.SourceMessageID)
	return t, nil
}

func (s *Store) SetStatus(id, status, errText string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=?, error=?, updated_at=? WHERE id=?`, status, errText, nowText(), normalizeID(id))
	return err
}

func (s *Store) SetSession(id, sessionID string) error {
	_, err := s.db.Exec(`UPDATE tasks SET agent_session_id=?, updated_at=? WHERE id=?`, sessionID, nowText(), normalizeID(id))
	return err
}

func (s *Store) SetWaiting(id, sessionID, question string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=?, agent_session_id=?, pending_question=?, updated_at=? WHERE id=?`, StatusWaitingUser, sessionID, question, nowText(), normalizeID(id))
	return err
}

func (s *Store) SetResult(id, status, result, errText string) error {
	_, err := s.db.Exec(`UPDATE tasks SET status=?, result=?, error=?, pending_question='', updated_at=? WHERE id=?`, status, result, errText, nowText(), normalizeID(id))
	return err
}

func (s *Store) AddMessage(taskID, direction, kind, content, externalID string) error {
	_, err := s.db.Exec(`INSERT INTO task_messages(task_id,direction,kind,content,external_id,created_at) VALUES(?,?,?,?,?,?)`, normalizeID(taskID), direction, kind, content, externalID, nowText())
	return err
}

func (s *Store) Task(id string) (Task, error) {
	row := s.db.QueryRow(`SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at FROM tasks WHERE id=?`, normalizeID(id))
	return scanTask(row)
}

func (s *Store) WaitingFor(sender string) ([]Task, error) {
	rows, err := s.db.Query(`SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at FROM tasks WHERE sender=? AND status=? ORDER BY updated_at`, sender, StatusWaitingUser)
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

func (s *Store) ActiveFor(sender string) ([]Task, error) {
	query := `SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at FROM tasks WHERE sender=? AND status IN (?,?,?,?,?) ORDER BY updated_at DESC`
	rows, err := s.db.Query(query, sender, StatusQueued, StatusRunning, StatusWaitingUser, StatusResuming, StatusResumeFailed)
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

func (s *Store) ExpireWaiting(before time.Time) ([]Task, error) {
	rows, err := s.db.Query(`SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at FROM tasks WHERE status=? AND updated_at<?`, StatusWaitingUser, before.UTC().Format(time.RFC3339Nano))
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, t := range out {
		_ = s.SetStatus(t.ID, StatusFailed, "等待用户回答超时")
	}
	return out, nil
}

func (s *Store) GetChannelState(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM channel_state WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) SetChannelState(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO channel_state(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, key, value, nowText())
	return err
}

func (s *Store) ResetIlinkState() error {
	_, err := s.db.Exec(`DELETE FROM channel_state WHERE key='ilink.cursor' OR key LIKE 'ilink.context.%'`)
	return err
}

func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		result[status] = count
	}
	return result, rows.Err()
}

func (s *Store) ImportLegacy(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy struct {
		Processed map[string]struct {
			Status    string    `json:"status"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"processed"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("导入 0.1 状态: %w", err)
	}
	for id, record := range legacy.Processed {
		at := record.UpdatedAt
		if at.IsZero() {
			at = time.Now()
		}
		_, err := s.db.Exec(`INSERT OR IGNORE INTO inbound_messages(channel,message_id,sender,body,disposition,received_at) VALUES('wechatian-files',?,?,'',?,?)`, id, "wechatian-owner", record.Status, at.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
	}
	return nil
}

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (Task, error) {
	var t Task
	var created, updated string
	err := row.Scan(&t.ID, &t.SourceMessageID, &t.Channel, &t.Sender, &t.Conversation, &t.Agent, &t.Project, &t.ProjectPath, &t.Prompt, &t.Status, &t.AgentSessionID, &t.PendingQuestion, &t.Result, &t.Error, &created, &updated)
	if err != nil {
		return t, err
	}
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return t, nil
}

func newTaskID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "T-" + strings.ToUpper(hex.EncodeToString(b))
}
func normalizeID(id string) string { return strings.ToUpper(strings.TrimSpace(id)) }
func nowText() string              { return time.Now().UTC().Format(time.RFC3339Nano) }
