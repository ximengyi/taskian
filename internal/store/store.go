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
	"strconv"
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
	ProjectID                                          int64
	Agent, Project, ProjectPath, Prompt, Status        string
	AgentSessionID, PendingQuestion, Result, Error     string
	CreatedAt, UpdatedAt                               time.Time
}

type Message struct {
	ID                                           int64
	TaskID, Direction, Kind, Content, ExternalID string
	CreatedAt                                    time.Time
}

type Project struct {
	ID                               int64
	Name, Path                       string
	CreatedAt, UpdatedAt, LastUsedAt time.Time
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
		`CREATE TABLE IF NOT EXISTS projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE COLLATE NOCASE, path TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_used_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS conversation_preferences (
			channel TEXT NOT NULL, conversation TEXT NOT NULL, current_project TEXT NOT NULL DEFAULT '',
			default_agent TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL,
			PRIMARY KEY(channel, conversation))`,
		`CREATE TABLE IF NOT EXISTS global_preferences (
			id INTEGER PRIMARY KEY CHECK(id=1), active_project_id INTEGER, updated_at TEXT NOT NULL,
			FOREIGN KEY(active_project_id) REFERENCES projects(id) ON DELETE SET NULL)`,
		`CREATE TABLE IF NOT EXISTS sender_preferences (
			sender TEXT PRIMARY KEY, active_project_id INTEGER, updated_at TEXT NOT NULL,
			FOREIGN KEY(active_project_id) REFERENCES projects(id) ON DELETE SET NULL)`,
		`CREATE TABLE IF NOT EXISTS selection_contexts (
			channel TEXT NOT NULL, sender TEXT NOT NULL, kind TEXT NOT NULL, expires_at TEXT NOT NULL,
			PRIMARY KEY(channel,sender))`,
		`CREATE TABLE IF NOT EXISTS channel_bindings (
			channel TEXT NOT NULL, platform_user_id TEXT NOT NULL, role TEXT NOT NULL, created_at TEXT NOT NULL,
			PRIMARY KEY(channel,platform_user_id))`,
	}
	if err := s.migrateProjects(); err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("初始化 SQLite: %w", err)
		}
	}
	if err := s.addColumnIfMissing("tasks", "project_id", `ALTER TABLE tasks ADD COLUMN project_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE tasks SET project_id=COALESCE((SELECT id FROM projects WHERE projects.name=tasks.project COLLATE NOCASE),0) WHERE project_id=0`); err != nil {
		return fmt.Errorf("回填任务项目 ID: %w", err)
	}
	return nil
}

func (s *Store) migrateProjects() error {
	rows, err := s.db.Query(`PRAGMA table_info(projects)`)
	if err != nil {
		return err
	}
	hasID, exists := false, false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		exists = true
		if name == "id" {
			hasID = true
		}
	}
	_ = rows.Close()
	if !exists || hasID {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []string{
		`ALTER TABLE projects RENAME TO projects_v04`,
		`CREATE TABLE projects (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE COLLATE NOCASE, path TEXT NOT NULL UNIQUE, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_used_at TEXT NOT NULL)`,
		`INSERT INTO projects(name,path,created_at,updated_at,last_used_at) SELECT name,path,created_at,updated_at,last_used_at FROM projects_v04 ORDER BY created_at,name`,
		`DROP TABLE projects_v04`,
	}
	for _, step := range steps {
		if _, err := tx.Exec(step); err != nil {
			return fmt.Errorf("迁移 0.4 项目表: %w", err)
		}
	}
	return tx.Commit()
}

func (s *Store) addColumnIfMissing(table, column, statement string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	_, err = s.db.Exec(statement)
	return err
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
	_, err := s.db.Exec(`INSERT INTO tasks(id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,created_at,updated_at,project_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.SourceMessageID, t.Channel, t.Sender, t.Conversation, t.Agent, t.Project, t.ProjectPath, t.Prompt, t.Status, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), t.ProjectID)
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
	row := s.db.QueryRow(`SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at,project_id FROM tasks WHERE id=?`, normalizeID(id))
	return scanTask(row)
}

func (s *Store) WaitingFor(sender string) ([]Task, error) {
	rows, err := s.db.Query(`SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at,project_id FROM tasks WHERE sender=? AND status=? ORDER BY updated_at`, sender, StatusWaitingUser)
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
	query := `SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at,project_id FROM tasks WHERE sender=? AND status IN (?,?,?,?,?) ORDER BY updated_at DESC`
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
	rows, err := s.db.Query(`SELECT id,source_message_id,channel,sender,conversation,agent,project,project_path,prompt,status,agent_session_id,pending_question,result,error,created_at,updated_at,project_id FROM tasks WHERE status=? AND updated_at<?`, StatusWaitingUser, before.UTC().Format(time.RFC3339Nano))
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

func (s *Store) PutProject(name, path string) error {
	_, err := s.PutProjectRecord(name, path)
	return err
}

func (s *Store) PutProjectRecord(name, path string) (Project, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	now := nowText()
	_, err := s.db.Exec(`INSERT INTO projects(name,path,created_at,updated_at,last_used_at) VALUES(?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET path=excluded.path,updated_at=excluded.updated_at`, name, path, now, now, now)
	if err != nil {
		return Project{}, err
	}
	return s.Project(name)
}

func (s *Store) Project(reference string) (Project, error) {
	var p Project
	var created, updated, used string
	reference = strings.TrimSpace(reference)
	query, argument := `SELECT id,name,path,created_at,updated_at,last_used_at FROM projects WHERE name=?`, any(strings.ToLower(reference))
	if id, err := strconv.ParseInt(reference, 10, 64); err == nil && id > 0 {
		query, argument = `SELECT id,name,path,created_at,updated_at,last_used_at FROM projects WHERE id=?`, id
	}
	err := s.db.QueryRow(query, argument).Scan(&p.ID, &p.Name, &p.Path, &created, &updated, &used)
	if err != nil {
		return p, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	p.LastUsedAt, _ = time.Parse(time.RFC3339Nano, used)
	return p, nil
}

func (s *Store) ProjectByPath(path string) (Project, error) {
	var p Project
	var created, updated, used string
	err := s.db.QueryRow(`SELECT id,name,path,created_at,updated_at,last_used_at FROM projects WHERE path=?`, path).Scan(&p.ID, &p.Name, &p.Path, &created, &updated, &used)
	if err != nil {
		return p, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	p.LastUsedAt, _ = time.Parse(time.RFC3339Nano, used)
	return p, nil
}

func (s *Store) Projects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id,name,path,created_at,updated_at,last_used_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Project
	for rows.Next() {
		var p Project
		var created, updated, used string
		if err := rows.Scan(&p.ID, &p.Name, &p.Path, &created, &updated, &used); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		p.LastUsedAt, _ = time.Parse(time.RFC3339Nano, used)
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) RemoveProject(reference string) error {
	p, err := s.Project(reference)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM projects WHERE id=?`, p.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`UPDATE conversation_preferences SET current_project='',updated_at=? WHERE current_project=?`, nowText(), p.Name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RenameProject(reference, newName string) error {
	p, err := s.Project(reference)
	if err != nil {
		return err
	}
	newName = strings.ToLower(strings.TrimSpace(newName))
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE projects SET name=?,updated_at=? WHERE id=?`, newName, nowText(), p.ID)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`UPDATE conversation_preferences SET current_project=?,updated_at=? WHERE current_project=?`, newName, nowText(), p.Name); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) TouchProject(reference string) error {
	p, err := s.Project(reference)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE projects SET last_used_at=? WHERE id=?`, nowText(), p.ID)
	return err
}

func (s *Store) UpdateProjectPath(reference, path string) (Project, error) {
	p, err := s.Project(reference)
	if err != nil {
		return Project{}, err
	}
	if _, err := s.db.Exec(`UPDATE projects SET path=?,updated_at=? WHERE id=?`, path, nowText(), p.ID); err != nil {
		return Project{}, err
	}
	return s.Project(strconv.FormatInt(p.ID, 10))
}

func (s *Store) SetActiveProject(id int64) error {
	_, err := s.db.Exec(`INSERT INTO global_preferences(id,active_project_id,updated_at) VALUES(1,?,?)
		ON CONFLICT(id) DO UPDATE SET active_project_id=excluded.active_project_id,updated_at=excluded.updated_at`, id, nowText())
	return err
}

func (s *Store) ActiveProject() (Project, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT active_project_id FROM global_preferences WHERE id=1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || !id.Valid || id.Int64 == 0 {
		return Project{}, sql.ErrNoRows
	}
	if err != nil {
		return Project{}, err
	}
	return s.Project(strconv.FormatInt(id.Int64, 10))
}

func (s *Store) SetSenderProject(sender string, id int64) error {
	_, err := s.db.Exec(`INSERT INTO sender_preferences(sender,active_project_id,updated_at) VALUES(?,?,?)
		ON CONFLICT(sender) DO UPDATE SET active_project_id=excluded.active_project_id,updated_at=excluded.updated_at`, sender, id, nowText())
	return err
}

func (s *Store) SenderProject(sender string) (Project, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT active_project_id FROM sender_preferences WHERE sender=?`, sender).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || !id.Valid || id.Int64 == 0 {
		return Project{}, sql.ErrNoRows
	}
	if err != nil {
		return Project{}, err
	}
	return s.Project(strconv.FormatInt(id.Int64, 10))
}

func (s *Store) SetSelectionContext(channel, sender, kind string, expires time.Time) error {
	_, err := s.db.Exec(`INSERT INTO selection_contexts(channel,sender,kind,expires_at) VALUES(?,?,?,?)
		ON CONFLICT(channel,sender) DO UPDATE SET kind=excluded.kind,expires_at=excluded.expires_at`, channel, sender, kind, expires.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) SelectionContext(channel, sender string, now time.Time) (string, bool, error) {
	var kind, expires string
	err := s.db.QueryRow(`SELECT kind,expires_at FROM selection_contexts WHERE channel=? AND sender=?`, channel, sender).Scan(&kind, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	deadline, _ := time.Parse(time.RFC3339Nano, expires)
	if !deadline.After(now.UTC()) {
		_, _ = s.db.Exec(`DELETE FROM selection_contexts WHERE channel=? AND sender=?`, channel, sender)
		return "", false, nil
	}
	return kind, true, nil
}

func (s *Store) ClearSelectionContext(channel, sender string) error {
	_, err := s.db.Exec(`DELETE FROM selection_contexts WHERE channel=? AND sender=?`, channel, sender)
	return err
}

func (s *Store) PutChannelBinding(channel, userID, role string) error {
	_, err := s.db.Exec(`INSERT INTO channel_bindings(channel,platform_user_id,role,created_at) VALUES(?,?,?,?)
		ON CONFLICT(channel,platform_user_id) DO UPDATE SET role=excluded.role`, channel, userID, role, nowText())
	return err
}

func (s *Store) ChannelBinding(channel, userID string) (string, error) {
	var role string
	err := s.db.QueryRow(`SELECT role FROM channel_bindings WHERE channel=? AND platform_user_id=?`, channel, userID).Scan(&role)
	return role, err
}

func (s *Store) DeleteChannelBindings(channel string) error {
	_, err := s.db.Exec(`DELETE FROM channel_bindings WHERE channel=?`, channel)
	return err
}

func (s *Store) SetConversationProject(channel, conversation, project string) error {
	_, err := s.db.Exec(`INSERT INTO conversation_preferences(channel,conversation,current_project,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(channel,conversation) DO UPDATE SET current_project=excluded.current_project,updated_at=excluded.updated_at`, channel, conversation, strings.ToLower(project), nowText())
	return err
}

func (s *Store) ConversationProject(channel, conversation string) (string, error) {
	var project string
	err := s.db.QueryRow(`SELECT current_project FROM conversation_preferences WHERE channel=? AND conversation=?`, channel, conversation).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return project, err
}

func (s *Store) MostRecentConversationProject() (Project, error) {
	var reference string
	err := s.db.QueryRow(`SELECT current_project FROM conversation_preferences WHERE current_project<>'' ORDER BY updated_at DESC LIMIT 1`).Scan(&reference)
	if err != nil {
		return Project{}, err
	}
	return s.Project(reference)
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
	err := row.Scan(&t.ID, &t.SourceMessageID, &t.Channel, &t.Sender, &t.Conversation, &t.Agent, &t.Project, &t.ProjectPath, &t.Prompt, &t.Status, &t.AgentSessionID, &t.PendingQuestion, &t.Result, &t.Error, &created, &updated, &t.ProjectID)
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
