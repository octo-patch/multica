package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// createTestTask creates an issue + dispatched task in the test workspace and
// returns (issueID, taskID). The caller is responsible for cleanup.
func createTestTask(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	// Get the next issue number for the workspace.
	var nextNum int
	testPool.QueryRow(ctx, `SELECT COALESCE(MAX(number), 0) + 1 FROM issue WHERE workspace_id = $1`, testWorkspaceID).Scan(&nextNum)

	var issueID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_type, creator_id, number)
		VALUES ($1, 'daemon auth test issue', 'todo', 'medium', 'member', $2, $3)
		RETURNING id
	`, testWorkspaceID, testUserID, nextNum).Scan(&issueID)
	if err != nil {
		t.Fatalf("create test issue: %v", err)
	}

	// Need an agent and its runtime in the workspace.
	var agentID, runtimeID string
	err = testPool.QueryRow(ctx, `
		SELECT a.id, a.runtime_id FROM agent a WHERE a.workspace_id = $1 LIMIT 1
	`, testWorkspaceID).Scan(&agentID, &runtimeID)
	if err != nil {
		t.Fatalf("find test agent: %v", err)
	}

	var taskID string
	err = testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, issue_id, status, priority, runtime_id)
		VALUES ($1, $2, 'dispatched', 0, $3)
		RETURNING id
	`, agentID, issueID, runtimeID).Scan(&taskID)
	if err != nil {
		t.Fatalf("create test task: %v", err)
	}

	return issueID, taskID
}

func cleanupTestTask(t *testing.T, issueID string) {
	t.Helper()
	ctx := context.Background()
	testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
	testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID)
}

// createOtherWorkspaceUser creates a user+workspace that is separate from the
// test workspace, for cross-tenant tests. Returns (userID, workspaceID).
func createOtherWorkspaceUser(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()

	var otherUserID string
	err := testPool.QueryRow(ctx, `
		INSERT INTO "user" (name, email)
		VALUES ('Other User', 'other-daemon-test@multica.ai')
		RETURNING id
	`).Scan(&otherUserID)
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	var otherWorkspaceID string
	err = testPool.QueryRow(ctx, `
		INSERT INTO workspace (name, slug, description, issue_prefix)
		VALUES ('Other Workspace', 'other-daemon-test', 'Cross-tenant test', 'OTH')
		RETURNING id
	`).Scan(&otherWorkspaceID)
	if err != nil {
		t.Fatalf("create other workspace: %v", err)
	}

	_, err = testPool.Exec(ctx, `
		INSERT INTO member (workspace_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, otherWorkspaceID, otherUserID)
	if err != nil {
		t.Fatalf("create other membership: %v", err)
	}

	return otherUserID, otherWorkspaceID
}

func cleanupOtherWorkspaceUser(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	testPool.Exec(ctx, `DELETE FROM workspace WHERE slug = 'other-daemon-test'`)
	testPool.Exec(ctx, `DELETE FROM "user" WHERE email = 'other-daemon-test@multica.ai'`)
}

func daemonRequest(method, path, userID string, body any) *http.Request {
	req := newRequest(method, path, body)
	req.Header.Set("X-User-ID", userID)
	req.Header.Del("X-Workspace-ID") // daemon routes don't use workspace header
	return req
}

func TestStartTask_SameWorkspace(t *testing.T) {
	issueID, taskID := createTestTask(t)
	defer cleanupTestTask(t, issueID)

	w := httptest.NewRecorder()
	req := daemonRequest("POST", "/api/daemon/tasks/"+taskID+"/start", testUserID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.StartTask(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("StartTask same workspace: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStartTask_CrossWorkspace(t *testing.T) {
	issueID, taskID := createTestTask(t)
	defer cleanupTestTask(t, issueID)

	otherUserID, _ := createOtherWorkspaceUser(t)
	defer cleanupOtherWorkspaceUser(t)

	w := httptest.NewRecorder()
	req := daemonRequest("POST", "/api/daemon/tasks/"+taskID+"/start", otherUserID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.StartTask(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("StartTask cross workspace: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCompleteTask_CrossWorkspace(t *testing.T) {
	issueID, taskID := createTestTask(t)
	defer cleanupTestTask(t, issueID)

	// Start the task first so it's in "running" state.
	ctx := context.Background()
	testPool.Exec(ctx, `UPDATE agent_task_queue SET status = 'running', started_at = now() WHERE id = $1`, taskID)

	otherUserID, _ := createOtherWorkspaceUser(t)
	defer cleanupOtherWorkspaceUser(t)

	w := httptest.NewRecorder()
	req := daemonRequest("POST", "/api/daemon/tasks/"+taskID+"/complete", otherUserID, TaskCompleteRequest{
		Output: "malicious output",
		PRURL:  "https://evil.com/pr",
	})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.CompleteTask(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("CompleteTask cross workspace: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Verify the task was NOT completed.
	var status string
	testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	if status != "running" {
		t.Fatalf("Task should still be running, got '%s'", status)
	}
}

func TestFailTask_CrossWorkspace(t *testing.T) {
	issueID, taskID := createTestTask(t)
	defer cleanupTestTask(t, issueID)

	otherUserID, _ := createOtherWorkspaceUser(t)
	defer cleanupOtherWorkspaceUser(t)

	w := httptest.NewRecorder()
	req := daemonRequest("POST", "/api/daemon/tasks/"+taskID+"/fail", otherUserID, TaskFailRequest{
		Error: "malicious error",
	})
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.FailTask(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("FailTask cross workspace: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTaskStatus_CrossWorkspace(t *testing.T) {
	issueID, taskID := createTestTask(t)
	defer cleanupTestTask(t, issueID)

	otherUserID, _ := createOtherWorkspaceUser(t)
	defer cleanupOtherWorkspaceUser(t)

	w := httptest.NewRecorder()
	req := daemonRequest("GET", "/api/daemon/tasks/"+taskID+"/status", otherUserID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.GetTaskStatus(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GetTaskStatus cross workspace: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestStartTask_NonexistentTask(t *testing.T) {
	w := httptest.NewRecorder()
	fakeID := "00000000-0000-0000-0000-000000000000"
	req := daemonRequest("POST", "/api/daemon/tasks/"+fakeID+"/start", testUserID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("taskId", fakeID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))

	testHandler.StartTask(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("StartTask nonexistent: expected 404, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] != "task not found" {
		t.Fatalf("Expected generic 'task not found' error, got '%s'", resp["error"])
	}
}
