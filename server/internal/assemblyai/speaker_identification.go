package assemblyai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAPIBaseURL = "https://api.assemblyai.com"

type SpeakerCandidate struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type IdentificationConfig struct {
	SpeakerType string             `json:"speaker_type"`
	Speakers    []SpeakerCandidate `json:"speakers"`
}

type IdentifiedUtterance struct {
	Speaker    string  `json:"speaker"`
	Text       string  `json:"text"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Confidence float64 `json:"confidence"`
}

type IdentificationResult struct {
	Status     string                `json:"status"`
	Mapping    map[string]string     `json:"mapping,omitempty"`
	Utterances []IdentifiedUtterance `json:"utterances,omitempty"`
	Error      string                `json:"error,omitempty"`
}

type SpeakerIdentificationClient interface {
	Create(context.Context, io.Reader, IdentificationConfig) (string, error)
	Query(context.Context, string) (IdentificationResult, error)
}

type SpeakerIdentificationService struct {
	APIKey     string
	APIBaseURL string
	HTTPClient *http.Client
}

func NewSpeakerIdentificationService() *SpeakerIdentificationService {
	return &SpeakerIdentificationService{
		APIKey:     strings.TrimSpace(os.Getenv("ASSEMBLYAI_API_KEY")),
		APIBaseURL: defaultAPIBaseURL,
		HTTPClient: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (s SpeakerIdentificationService) Create(ctx context.Context, audio io.Reader, config IdentificationConfig) (string, error) {
	if strings.TrimSpace(s.APIKey) == "" {
		return "", ErrAPIKeyNotConfigured
	}
	uploadURL, err := s.upload(ctx, audio)
	if err != nil {
		return "", err
	}
	speakers := make([]map[string]string, 0, len(config.Speakers))
	for _, candidate := range config.Speakers {
		item := map[string]string{config.SpeakerType: candidate.Value}
		if candidate.Description != "" {
			item["description"] = candidate.Description
		}
		speakers = append(speakers, item)
	}
	body := map[string]any{
		"audio_url":      uploadURL,
		"speaker_labels": true,
		"speech_understanding": map[string]any{"request": map[string]any{
			"speaker_identification": map[string]any{
				"speaker_type": config.SpeakerType,
				"speakers":     speakers,
				"effort":       "low",
			},
		}},
	}
	var response struct {
		ID string `json:"id"`
	}
	if err := s.jsonRequest(ctx, http.MethodPost, "/v2/transcript", body, &response); err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", errors.New("AssemblyAI transcript response did not contain id")
	}
	return response.ID, nil
}

func (s SpeakerIdentificationService) upload(ctx context.Context, audio io.Reader) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL()+"/v2/upload", audio)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", s.APIKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("upload audio to AssemblyAI: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", upstreamStatusError("upload", resp)
	}
	var parsed struct {
		UploadURL string `json:"upload_url"`
	}
	if err := decodeLimited(resp.Body, &parsed); err != nil {
		return "", fmt.Errorf("decode AssemblyAI upload response: %w", err)
	}
	if parsed.UploadURL == "" {
		return "", errors.New("AssemblyAI upload response did not contain upload_url")
	}
	return parsed.UploadURL, nil
}

func (s SpeakerIdentificationService) Query(ctx context.Context, transcriptID string) (IdentificationResult, error) {
	if strings.TrimSpace(s.APIKey) == "" {
		return IdentificationResult{}, ErrAPIKeyNotConfigured
	}
	var response struct {
		Status              string `json:"status"`
		Error               string `json:"error"`
		SpeechUnderstanding struct {
			Response struct {
				SpeakerIdentification struct {
					Status  string            `json:"status"`
					Mapping map[string]string `json:"mapping"`
				} `json:"speaker_identification"`
			} `json:"response"`
		} `json:"speech_understanding"`
		Utterances []IdentifiedUtterance `json:"utterances"`
	}
	if err := s.jsonRequest(ctx, http.MethodGet, "/v2/transcript/"+transcriptID, nil, &response); err != nil {
		return IdentificationResult{}, err
	}
	status := response.Status
	if status == "queued" {
		status = "processing"
	}
	if status == "error" {
		status = "failed"
	}
	return IdentificationResult{
		Status:     status,
		Mapping:    response.SpeechUnderstanding.Response.SpeakerIdentification.Mapping,
		Utterances: response.Utterances,
		Error:      response.Error,
	}, nil
}

func (s SpeakerIdentificationService) jsonRequest(ctx context.Context, method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL()+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", s.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("request AssemblyAI: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return upstreamStatusError(path, resp)
	}
	if err := decodeLimited(resp.Body, target); err != nil {
		return fmt.Errorf("decode AssemblyAI response: %w", err)
	}
	return nil
}

func (s SpeakerIdentificationService) baseURL() string {
	if strings.TrimSpace(s.APIBaseURL) == "" {
		return defaultAPIBaseURL
	}
	return strings.TrimRight(s.APIBaseURL, "/")
}

func (s SpeakerIdentificationService) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return http.DefaultClient
}

func decodeLimited(reader io.Reader, target any) error {
	return json.NewDecoder(io.LimitReader(reader, 2<<20)).Decode(target)
}

func upstreamStatusError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("AssemblyAI %s failed with status %d: %s", operation, resp.StatusCode, strings.TrimSpace(string(body)))
}
