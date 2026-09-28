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
	// One-shot pre-recorded create: WAV is already uploaded, so speaker
	// identification must ride along on POST /v2/transcript. Do not call
	// llm-gateway /v1/understanding here — that path requires an existing
	// completed pre-recorded transcript ID, and a streaming session ID is not
	// interchangeable with one.
	body := map[string]any{
		"audio_url":          uploadURL,
		"language_detection": true,
		"speaker_labels":     true,
		// The pre-recorded pass must preserve the configured participant
		// count. Without this constraint short meeting audio can collapse the
		// real-time A/B turns into a single diarized speaker.
		"speakers_expected": len(config.Speakers),
		"speech_understanding": map[string]any{"request": map[string]any{
			"speaker_identification": map[string]any{
				"speaker_type": config.SpeakerType,
				"speakers":     speakers,
				// AssemblyAI recommends medium effort for meeting-room audio.
				"effort": "medium",
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
					Error   string            `json:"error"`
				} `json:"speaker_identification"`
			} `json:"response"`
		} `json:"speech_understanding"`
		Utterances []IdentifiedUtterance `json:"utterances"`
	}
	if err := s.jsonRequest(ctx, http.MethodGet, "/v2/transcript/"+transcriptID, nil, &response); err != nil {
		return IdentificationResult{}, err
	}
	identification := response.SpeechUnderstanding.Response.SpeakerIdentification
	status, errMsg := identificationJobStatus(
		response.Status,
		identification.Status,
		identification.Mapping,
		response.Utterances,
		response.Error,
		identification.Error,
	)
	utterances := response.Utterances
	if status == "completed" {
		utterances = applySpeakerMapping(utterances, identification.Mapping)
	}
	return IdentificationResult{
		Status:     status,
		Mapping:    identification.Mapping,
		Utterances: utterances,
		Error:      errMsg,
	}, nil
}

func identificationJobStatus(
	transcriptStatus, identificationStatus string,
	mapping map[string]string,
	utterances []IdentifiedUtterance,
	transcriptErr, identificationErr string,
) (string, string) {
	switch strings.ToLower(strings.TrimSpace(transcriptStatus)) {
	case "queued", "processing":
		return "processing", ""
	case "error":
		return "failed", firstNonEmpty(transcriptErr, "AssemblyAI transcription failed")
	case "completed":
		switch strings.ToLower(strings.TrimSpace(identificationStatus)) {
		case "error", "failed":
			return "failed", firstNonEmpty(
				identificationErr,
				transcriptErr,
				"AssemblyAI speaker identification did not succeed",
			)
		case "success":
			return "completed", ""
		default:
			// Identification is a nested speech-understanding task. The
			// pre-recorded transcript can complete before mapping/utterances
			// are rewritten; keep polling instead of failing the job.
			if len(usableSpeakerMapping(mapping)) > 0 || utterancesHaveIdentities(utterances) {
				return "completed", ""
			}
			return "processing", ""
		}
	default:
		return "processing", ""
	}
}

func applySpeakerMapping(utterances []IdentifiedUtterance, mapping map[string]string) []IdentifiedUtterance {
	normalized := usableSpeakerMapping(mapping)
	if len(utterances) == 0 || len(normalized) == 0 {
		return utterances
	}
	out := make([]IdentifiedUtterance, len(utterances))
	for i, utterance := range utterances {
		out[i] = utterance
		if name, ok := normalized[speakerKey(utterance.Speaker)]; ok {
			out[i].Speaker = name
		}
	}
	return out
}

func usableSpeakerMapping(mapping map[string]string) map[string]string {
	normalized := make(map[string]string, len(mapping))
	for key, value := range mapping {
		label := speakerKey(key)
		name := strings.TrimSpace(value)
		if label != "" && name != "" {
			normalized[label] = name
		}
	}
	return normalized
}

func utterancesHaveIdentities(utterances []IdentifiedUtterance) bool {
	for _, utterance := range utterances {
		if !isGenericSpeakerLabel(utterance.Speaker) {
			return true
		}
	}
	return false
}

func isGenericSpeakerLabel(label string) bool {
	key := speakerKey(label)
	return key == "" || (len(key) == 1 && key[0] >= 'A' && key[0] <= 'Z')
}

func speakerKey(label string) string {
	value := strings.TrimSpace(label)
	upper := strings.ToUpper(value)
	if strings.HasPrefix(upper, "SPEAKER") {
		value = strings.TrimSpace(value[len("SPEAKER"):])
	}
	return strings.ToUpper(strings.TrimSpace(value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
