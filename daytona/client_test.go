package daytona

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helper: spin up an httptest.Server with the supplied handler and a
// Client pointed at it. Returns both for assertion / cleanup.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, "test-api-key", srv.Client())
	return c, srv
}

func TestClient_AuthHeaderSentOnEveryRequest(t *testing.T) {
	var seenAuth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sb-1","state":"started"}`))
	})

	if _, err := c.GetSandbox(context.Background(), "sb-1"); err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if want := "Bearer test-api-key"; seenAuth != want {
		t.Fatalf("Authorization header = %q, want %q", seenAuth, want)
	}
}

func TestClient_CreateSandbox_HappyPath(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sandbox" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var body CreateSandboxRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Labels["surf-name"] != "task-42" {
			t.Errorf("label surf-name = %q, want task-42", body.Labels["surf-name"])
		}
		if body.AutoStopInterval == nil || *body.AutoStopInterval != 0 {
			t.Errorf("autoStopInterval = %v, want pointer to 0", body.AutoStopInterval)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sb-new","state":"creating","labels":{"surf-name":"task-42"}}`))
	})

	zero := 0
	got, err := c.CreateSandbox(context.Background(), CreateSandboxRequest{
		Labels:           map[string]string{"surf-name": "task-42"},
		AutoStopInterval: &zero,
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if got.ID != "sb-new" || got.State != "creating" {
		t.Errorf("got = %+v", got)
	}
}

func TestClient_GetSandbox_NotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, err := c.GetSandbox(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestClient_GetSandbox_APIError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	})

	_, err := c.GetSandbox(context.Background(), "sb-1")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", apiErr.Status)
	}
	if !strings.Contains(apiErr.Body, "boom") {
		t.Errorf("body = %q, want to contain 'boom'", apiErr.Body)
	}
}

func TestClient_DeleteSandbox_UsesForceQuery(t *testing.T) {
	var seenURL string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenURL = r.URL.RequestURI()
		w.WriteHeader(http.StatusOK)
	})

	if err := c.DeleteSandbox(context.Background(), "sb-1"); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	if !strings.Contains(seenURL, "force=true") {
		t.Errorf("URL = %q, want to contain force=true", seenURL)
	}
}

func TestClient_ListSandboxes_BareArray(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
	})

	got, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("got = %+v", got)
	}
}

func TestClient_ListSandboxes_Wrapped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[{"id":"x"}]}`))
	})

	got, err := c.ListSandboxes(context.Background())
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x" {
		t.Errorf("got = %+v", got)
	}
}

func TestClient_GetPreviewURL(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sandbox/sb-1/ports/3000/preview-url" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://3000-x.daytonaproxy01.net","token":"tok-abc","sandboxId":"sb-1"}`))
	})

	got, err := c.GetPreviewURL(context.Background(), "sb-1", 3000)
	if err != nil {
		t.Fatalf("GetPreviewURL: %v", err)
	}
	if got.URL == "" || got.Token != "tok-abc" {
		t.Errorf("got = %+v", got)
	}
}

func TestClient_Execute(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/toolbox/sb-1/toolbox/process/execute" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["command"] != "echo hi" {
			t.Errorf("command = %q", body["command"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"exitCode":0,"result":"hi\n"}`))
	})

	got, err := c.Execute(context.Background(), "sb-1", "echo hi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.ExitCode != 0 || got.Result != "hi\n" {
		t.Errorf("got = %+v", got)
	}
}

func TestClient_Upload_UsesSingleFileEndpoint(t *testing.T) {
	var seenPath, seenQuery, seenContentType string
	var seenFilename string
	var seenFileBody string

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.Query().Get("path")
		seenContentType = r.Header.Get("Content-Type")

		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		fhs := r.MultipartForm.File["file"]
		if len(fhs) != 1 {
			t.Fatalf("file headers = %d, want 1", len(fhs))
		}
		seenFilename = fhs[0].Filename
		f, err := fhs[0].Open()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		seenFileBody = string(b)

		w.WriteHeader(http.StatusOK)
	})

	if err := c.Upload(context.Background(), "sb-1", "/home/daytona/manifest.json", []byte("hello")); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	wantPath := "/api/toolbox/sb-1/toolbox/files/upload"
	if seenPath != wantPath {
		t.Errorf("path = %q, want %q", seenPath, wantPath)
	}
	if seenQuery != "/home/daytona/manifest.json" {
		t.Errorf("path query = %q", seenQuery)
	}
	if !strings.HasPrefix(seenContentType, "multipart/form-data") {
		t.Errorf("Content-Type = %q, want multipart/form-data...", seenContentType)
	}
	if seenFilename != "manifest.json" {
		t.Errorf("filename = %q, want manifest.json (basename of dest)", seenFilename)
	}
	if seenFileBody != "hello" {
		t.Errorf("file body = %q, want %q", seenFileBody, "hello")
	}
}

func TestClient_Upload_RejectsEmptyPath(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not reach server when path is empty")
	})
	if err := c.Upload(context.Background(), "sb-1", "", []byte("x")); err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestClient_CreateSession(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/toolbox/sb-1/toolbox/process/session" {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["sessionId"] != "sess-1" {
			t.Errorf("sessionId = %q", body["sessionId"])
		}
		w.WriteHeader(http.StatusCreated)
	})
	if err := c.CreateSession(context.Background(), "sb-1", "sess-1"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
}

func TestClient_SessionExec_ReturnsCmdID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["command"] != "ls" {
			t.Errorf("command = %v", body["command"])
		}
		if body["runAsync"] != true {
			t.Errorf("runAsync = %v, want true", body["runAsync"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cmdId":"cmd-xyz"}`))
	})
	got, err := c.SessionExec(context.Background(), "sb-1", "sess-1", "ls", true)
	if err != nil {
		t.Fatalf("SessionExec: %v", err)
	}
	if got.CmdID != "cmd-xyz" {
		t.Errorf("cmdId = %q", got.CmdID)
	}
}

func TestClient_SessionLogs_StreamsBody(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("line1\nline2\n"))
	})
	rc, err := c.SessionLogs(context.Background(), "sb-1", "sess-1", "cmd-1")
	if err != nil {
		t.Fatalf("SessionLogs: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "line1\nline2\n" {
		t.Errorf("got = %q", got)
	}
}

func TestClient_NewClient_Defaults(t *testing.T) {
	c := NewClient("", "k", nil)
	if c.baseURL != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, DefaultBaseURL)
	}
	if c.hc == nil {
		t.Error("hc should default to non-nil")
	}
}

func TestClient_NewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://example.com/", "k", nil)
	if c.baseURL != "https://example.com" {
		t.Errorf("baseURL = %q, want trimmed", c.baseURL)
	}
}

// Sanity: multipart construction is deterministic enough that the helper
// produces a parseable body. (This is mainly a regression for the
// CreateFormFile usage rather than a behavioral test.)
func TestClient_Upload_MultipartIsParseable(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("media type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		_, err = mr.NextPart()
		if err != nil {
			t.Fatalf("NextPart: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Upload(context.Background(), "sb-1", "/x", []byte("y")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
}
