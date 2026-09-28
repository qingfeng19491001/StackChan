package assemblyai

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSpeakerIdentificationCreateAndQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" {
			t.Fatalf("missing upstream authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/upload":
			var body bytes.Buffer
			_, _ = body.ReadFrom(r.Body)
			if body.String() != "RIFF-test" {
				t.Fatalf("upload body=%q", body.String())
			}
			_, _ = w.Write([]byte(`{"upload_url":"https://upload.test/audio"}`))
		case "/v2/transcript":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["speaker_labels"] != true || body["language_detection"] != true || body["audio_url"] != "https://upload.test/audio" || body["speakers_expected"] != float64(2) {
				t.Fatalf("unexpected submit body: %#v", body)
			}
			understanding := body["speech_understanding"].(map[string]any)
			request := understanding["request"].(map[string]any)
			identification := request["speaker_identification"].(map[string]any)
			if identification["speaker_type"] != "name" || identification["effort"] != "medium" {
				t.Fatalf("unexpected identification: %#v", identification)
			}
			speakers := identification["speakers"].([]any)
			first := speakers[0].(map[string]any)
			if first["name"] != "张三" || first["description"] != "主持会议" {
				t.Fatalf("unexpected speaker: %#v", first)
			}
			second := speakers[1].(map[string]any)
			if second["name"] != "李四" || second["description"] != "项目负责人" {
				t.Fatalf("unexpected second speaker: %#v", second)
			}
			_, _ = w.Write([]byte(`{"id":"transcript-1","status":"queued"}`))
		case "/v2/transcript/transcript-1":
			_, _ = w.Write([]byte(`{
				"id":"transcript-1","status":"completed",
				"speech_understanding":{"response":{"speaker_identification":{"status":"success","mapping":{"A":"张三"}}}},
				"utterances":[{"speaker":"张三","text":"会议内容","start":1200,"end":4600,"confidence":0.97}]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	service := SpeakerIdentificationService{
		APIKey:     "test-key",
		APIBaseURL: server.URL,
		HTTPClient: server.Client(),
	}
	id, err := service.Create(context.Background(), bytes.NewReader([]byte("RIFF-test")), IdentificationConfig{
		SpeakerType: "name",
		Speakers: []SpeakerCandidate{
			{Value: "张三", Description: "主持会议"},
			{Value: "李四", Description: "项目负责人"},
		},
	})
	if err != nil || id != "transcript-1" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	result, err := service.Query(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Mapping["A"] != "张三" || len(result.Utterances) != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSpeakerIdentificationQueryPropagatesProcessingAndFailure(t *testing.T) {
	responses := []string{`{"status":"processing"}`, `{"status":"error","error":"bad audio"}`}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responses[0]))
		responses = responses[1:]
	}))
	defer server.Close()
	service := SpeakerIdentificationService{APIKey: "test-key", APIBaseURL: server.URL, HTTPClient: server.Client()}
	first, err := service.Query(context.Background(), "transcript-1")
	if err != nil || first.Status != "processing" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := service.Query(context.Background(), "transcript-1")
	if err != nil || second.Status != "failed" || second.Error != "bad audio" {
		t.Fatalf("second=%+v err=%v", second, err)
	}
}

func TestSpeakerIdentificationQueryKeepsPollingUntilIdentificationIsReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"completed",
			"utterances":[{"speaker":"A","text":"hello","start":0,"end":1000,"confidence":0.9}]
		}`))
	}))
	defer server.Close()

	service := SpeakerIdentificationService{APIKey: "test-key", APIBaseURL: server.URL, HTTPClient: server.Client()}
	result, err := service.Query(context.Background(), "transcript-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "processing" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSpeakerIdentificationQueryAppliesMappingToLetterUtterances(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"completed",
			"speech_understanding":{"response":{"speaker_identification":{"status":"success","mapping":{"A":"张三","B":"李四"}}}},
			"utterances":[{"speaker":"A","text":"hello","start":0,"end":1000,"confidence":0.9}]
		}`))
	}))
	defer server.Close()

	service := SpeakerIdentificationService{APIKey: "test-key", APIBaseURL: server.URL, HTTPClient: server.Client()}
	result, err := service.Query(context.Background(), "transcript-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Mapping["A"] != "张三" || result.Utterances[0].Speaker != "张三" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSpeakerIdentificationQueryCompletesWhenUtterancesAlreadyHaveIdentities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"completed",
			"utterances":[{"speaker":"张三","text":"hello","start":0,"end":1000,"confidence":0.9}]
		}`))
	}))
	defer server.Close()

	service := SpeakerIdentificationService{APIKey: "test-key", APIBaseURL: server.URL, HTTPClient: server.Client()}
	result, err := service.Query(context.Background(), "transcript-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Utterances[0].Speaker != "张三" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSpeakerIdentificationQueryRejectsFailedIdentificationOnCompletedTranscript(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"completed",
			"speech_understanding":{"response":{"speaker_identification":{"status":"error","error":"could not identify speakers"}}},
			"utterances":[{"speaker":"A","text":"hello","start":0,"end":1000,"confidence":0.9}]
		}`))
	}))
	defer server.Close()

	service := SpeakerIdentificationService{APIKey: "test-key", APIBaseURL: server.URL, HTTPClient: server.Client()}
	result, err := service.Query(context.Background(), "transcript-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.Error != "could not identify speakers" {
		t.Fatalf("unexpected result: %+v", result)
	}
}
