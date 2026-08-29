package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateVersion04ProjectsAssignsStableIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taskian.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE projects (name TEXT PRIMARY KEY COLLATE NOCASE, path TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, last_used_at TEXT NOT NULL);
		INSERT INTO projects VALUES ('first','/first','2025-01-01T00:00:00Z','2025-01-01T00:00:00Z','2025-01-01T00:00:00Z'),('second','/second','2025-01-02T00:00:00Z','2025-01-02T00:00:00Z','2025-01-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	state, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	first, _ := state.Project("first")
	second, _ := state.Project("2")
	if first.ID != 1 || second.Name != "second" || second.ID != 2 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestProjectSelectionPathAndExpiry(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	p, err := state.PutProjectRecord("demo", filepath.Join(t.TempDir(), "old"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.SetActiveProject(p.ID); err != nil {
		t.Fatal(err)
	}
	if current, err := state.ActiveProject(); err != nil || current.ID != p.ID {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	newPath := filepath.Join(t.TempDir(), "new")
	updated, err := state.UpdateProjectPath("1", newPath)
	if err != nil || updated.Path != newPath {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	if err := state.SetSelectionContext("feishu", "chat", "project", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, active, err := state.SelectionContext("feishu", "chat", time.Now()); err != nil || active {
		t.Fatalf("active=%v err=%v", active, err)
	}
}

func TestTaskLifecycleAndRestartRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taskian.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(Task{SourceMessageID: "m1", Channel: "test", Sender: "u1", Conversation: "u1", Agent: "codex", Project: "p", ProjectPath: t.TempDir(), Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(task.ID, StatusRunning, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSession(task.ID, "thread-1"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	got, err := s.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusResumeFailed {
		t.Fatalf("status=%s", got.Status)
	}
	if got.AgentSessionID != "thread-1" {
		t.Fatalf("session=%s", got.AgentSessionID)
	}
}

func TestExpireWaitingDoesNotDeadlock(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, err := s.CreateTask(Task{SourceMessageID: "m3", Channel: "test", Sender: "u", Conversation: "u", Agent: "codex", Project: "p", ProjectPath: t.TempDir(), Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWaiting(task.ID, "session", "question"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		expired, err := s.ExpireWaiting(time.Now().Add(time.Hour))
		if err == nil && len(expired) != 1 {
			err = errors.New("expected one expired task")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExpireWaiting deadlocked")
	}
}

func TestWaitingTaskSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taskian.db")
	s, _ := Open(path)
	task, err := s.CreateTask(Task{SourceMessageID: "m2", Channel: "test", Sender: "u1", Conversation: "u1", Agent: "cursor", Project: "p", ProjectPath: t.TempDir(), Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetWaiting(task.ID, "session-2", "choose"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusWaitingUser || got.PendingQuestion != "choose" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestProjectRegistryAndConversationPreference(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "taskian.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	path := filepath.Join(t.TempDir(), "week-report")
	if err := state.PutProject("Week-Report", path); err != nil {
		t.Fatal(err)
	}
	p, err := state.Project("week-report")
	if err != nil || p.Path != path || p.Name != "week-report" {
		t.Fatalf("project=%+v err=%v", p, err)
	}
	if err := state.SetConversationProject("ilink", "owner", "week-report"); err != nil {
		t.Fatal(err)
	}
	current, err := state.ConversationProject("ilink", "owner")
	if err != nil || current != "week-report" {
		t.Fatalf("current=%q err=%v", current, err)
	}
	if err := state.RenameProject("week-report", "weekly"); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Project("weekly"); err != nil {
		t.Fatal(err)
	}
	current, err = state.ConversationProject("ilink", "owner")
	if err != nil || current != "weekly" {
		t.Fatalf("renamed current=%q err=%v", current, err)
	}
	if err := state.RemoveProject("weekly"); err != nil {
		t.Fatal(err)
	}
	current, err = state.ConversationProject("ilink", "owner")
	if err != nil || current != "" {
		t.Fatalf("removed current=%q err=%v", current, err)
	}
}
