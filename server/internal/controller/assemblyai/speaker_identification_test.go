package assemblyai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	assemblyaiservice "stackChan/internal/assemblyai"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/util/guid"
)

type fakeIdentificationService struct{}

func (fakeIdentificationService) Create(_ context.Context, audio io.Reader, config assemblyaiservice.IdentificationConfig) (string, error) {
	body, _ := io.ReadAll(audio)
	if string(body) != "RIFF-test" || config.SpeakerType != "name" || config.Speakers[0].Value != "张三" {
		return "", fmt.Errorf("unexpected request")
	}
	return "transcript-1", nil
}

func (fakeIdentificationService) Query(_ context.Context, transcriptID string) (assemblyaiservice.IdentificationResult, error) {
	if transcriptID != "transcript-1" {
		return assemblyaiservice.IdentificationResult{}, fmt.Errorf("unexpected transcript")
	}
	return assemblyaiservice.IdentificationResult{
		Status:  "completed",
		Mapping: map[string]string{"A": "张三"},
		Utterances: []assemblyaiservice.IdentifiedUtterance{{
			Speaker: "张三", Text: "会议内容", Start: 100, End: 500,
		}},
	}, nil
}

func TestSpeakerIdentificationHTTPFlow(t *testing.T) {
	handlers := testSpeakerIdentificationHandlers(func(*ghttp.Request) (string, error) {
		return "user-1", nil
	})
	baseURL := startIdentificationServer(t, handlers)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("audio", "meeting.wav")
	_, _ = file.Write([]byte("RIFF-test"))
	_ = writer.WriteField("speaker_type", "name")
	_ = writer.WriteField("speakers", `[{"value":"张三","description":"主持会议"}]`)
	_ = writer.Close()
	request, _ := http.NewRequest(http.MethodPost, baseURL+"/jobs", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("create status=%d", response.StatusCode)
	}
	var created struct {
		Data struct {
			JobToken string `json:"job_token"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil || created.Data.JobToken == "" {
		t.Fatalf("create body=%+v err=%v", created, err)
	}

	queryBody, _ := json.Marshal(map[string]string{"job_token": created.Data.JobToken})
	queryResponse, err := http.Post(baseURL+"/jobs/query", "application/json", bytes.NewReader(queryBody))
	if err != nil {
		t.Fatal(err)
	}
	defer queryResponse.Body.Close()
	if queryResponse.StatusCode != http.StatusOK {
		t.Fatalf("query status=%d", queryResponse.StatusCode)
	}
	var queried map[string]any
	_ = json.NewDecoder(queryResponse.Body).Decode(&queried)
	data := queried["data"].(map[string]any)
	if data["status"] != "completed" {
		t.Fatalf("query body=%#v", queried)
	}
}

func TestSpeakerIdentificationRequiresAuthentication(t *testing.T) {
	handlers := testSpeakerIdentificationHandlers(func(*ghttp.Request) (string, error) {
		return "", fmt.Errorf("invalid credentials")
	})
	baseURL := startIdentificationServer(t, handlers)
	response, err := http.Post(baseURL+"/jobs/query", "application/json", bytes.NewBufferString(`{"job_token":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.StatusCode)
	}
}

func testSpeakerIdentificationHandlers(auth func(*ghttp.Request) (string, error)) SpeakerIdentificationHandlers {
	return SpeakerIdentificationHandlers{
		AuthenticateUser: auth,
		Service:          fakeIdentificationService{},
		Signer: assemblyaiservice.JobTokenSigner{
			Secret: []byte("32-byte-test-signing-secret-value"),
		},
		Now: func() time.Time { return time.Unix(1000, 0) },
	}
}

func startIdentificationServer(t *testing.T, handlers SpeakerIdentificationHandlers) string {
	t.Helper()
	server := g.Server(guid.S())
	server.SetAddr("127.0.0.1:0")
	server.SetDumpRouterMap(false)
	server.BindHandler("POST:/jobs", handlers.Create)
	server.BindHandler("POST:/jobs/query", handlers.Query)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown() })
	return fmt.Sprintf("http://127.0.0.1:%d", server.GetListenedPort())
}
