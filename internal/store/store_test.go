package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

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
