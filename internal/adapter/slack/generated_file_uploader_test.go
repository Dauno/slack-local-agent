package slack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
)

func TestGeneratedFileUploaderUsesExternalUploadFlow(t *testing.T) {
	var uploaded []byte
	var completedForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/files.getUploadURLExternal":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"file_id":"F123","upload_url":"` + serverURL(request) + `/upload"}`))
		case "/upload":
			if err := request.ParseMultipartForm(1024 * 1024); err != nil {
				t.Errorf("ParseMultipartForm() error: %v", err)
				return
			}
			file, _, err := request.FormFile("file")
			if err != nil {
				t.Errorf("FormFile() error: %v", err)
				return
			}
			defer file.Close()
			uploaded, _ = io.ReadAll(file)
			_, _ = w.Write([]byte("ok"))
		case "/files.completeUploadExternal":
			if err := request.ParseForm(); err != nil {
				t.Errorf("ParseForm() error: %v", err)
				return
			}
			completedForm = request.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"files":[{"id":"F123","title":"report.txt"}]}`))
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
		}
	}))
	defer server.Close()

	uploader := NewGeneratedFileUploader(slackapi.New("xoxb-test", slackapi.OptionAPIURL(server.URL+"/")), time.Second)
	target, err := uploader.RequestUploadURL(context.Background(), "report.txt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if target.FileID != "F123" || target.UploadURL != server.URL+"/upload" {
		t.Fatalf("target = %#v", target)
	}
	if err := uploader.UploadBytes(context.Background(), target, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != "hello" {
		t.Fatalf("uploaded = %q", uploaded)
	}
	if err := uploader.CompleteUpload(context.Background(), target.FileID, "C123", "1.2", "report.txt"); err != nil {
		t.Fatal(err)
	}
	if completedForm.Get("channel_id") != "C123" || completedForm.Get("thread_ts") != "1.2" || !strings.Contains(completedForm.Get("files"), `"id":"F123"`) {
		t.Fatalf("complete form = %#v", completedForm)
	}
}

func serverURL(request *http.Request) string {
	return "http://" + request.Host
}
