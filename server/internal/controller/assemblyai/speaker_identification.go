package assemblyai

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	assemblyaiservice "stackChan/internal/assemblyai"
	"stackChan/internal/pairing"

	"github.com/gogf/gf/v2/net/ghttp"
)

const (
	maxAudioBytes             = 150 << 20
	maxMultipartOverheadBytes = 1 << 20
	maxCandidates             = 10
	maxCandidateValueRunes    = 100
	maxDescriptionRunes       = 500
)

type SpeakerIdentificationHandlers struct {
	AuthenticateUser pairing.UserAuthenticator
	Service          assemblyaiservice.SpeakerIdentificationClient
	Signer           assemblyaiservice.JobTokenSigner
	Now              func() time.Time
}

func (h SpeakerIdentificationHandlers) Create(r *ghttp.Request) {
	userID, ok := h.authenticate(r)
	if !ok {
		return
	}
	r.Request.Body = http.MaxBytesReader(r.Response.Writer, r.Request.Body, maxAudioBytes+maxMultipartOverheadBytes)
	if err := r.Request.ParseMultipartForm(1 << 20); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "request body too large") {
			status = http.StatusRequestEntityTooLarge
		}
		h.writeError(r, status, "INVALID_MULTIPART", "invalid or oversized multipart request")
		return
	}
	if r.Request.MultipartForm != nil {
		defer r.Request.MultipartForm.RemoveAll()
	}
	audio, _, err := r.Request.FormFile("audio")
	if err != nil {
		h.writeError(r, http.StatusBadRequest, "MISSING_AUDIO", "audio WAV is required")
		return
	}
	defer audio.Close()

	speakerType := strings.TrimSpace(r.Request.FormValue("speaker_type"))
	var rawCandidates []struct {
		Value       string `json:"value"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(r.Request.FormValue("speakers")), &rawCandidates); err != nil {
		h.writeError(r, http.StatusBadRequest, "INVALID_SPEAKERS", "speakers must be a JSON array")
		return
	}
	config, err := validateIdentificationConfig(speakerType, rawCandidates)
	if err != nil {
		h.writeError(r, http.StatusBadRequest, "INVALID_CONFIG", err.Error())
		return
	}
	transcriptID, err := h.Service.Create(r.Context(), audio, config)
	if err != nil {
		status := http.StatusBadGateway
		code := "ASSEMBLYAI_CREATE_FAILED"
		if errors.Is(err, assemblyaiservice.ErrAPIKeyNotConfigured) {
			status = http.StatusServiceUnavailable
			code = "ASSEMBLYAI_NOT_CONFIGURED"
		}
		h.writeError(r, status, code, err.Error())
		return
	}
	jobToken, err := h.Signer.Sign(transcriptID, userID, h.now().Add(24*time.Hour))
	if err != nil {
		h.writeError(r, http.StatusServiceUnavailable, "JOB_SIGNING_UNAVAILABLE", "speaker identification signing is not configured")
		return
	}
	h.write(r, http.StatusAccepted, map[string]any{
		"code": 0, "message": "success",
		"data": map[string]any{"job_token": jobToken, "status": "queued"},
	})
}

func (h SpeakerIdentificationHandlers) Query(r *ghttp.Request) {
	userID, ok := h.authenticate(r)
	if !ok {
		return
	}
	var request struct {
		JobToken string `json:"job_token"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(r.Response.Writer, r.Request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.JobToken) == "" {
		h.writeError(r, http.StatusBadRequest, "INVALID_JOB_TOKEN", "job_token is required")
		return
	}
	transcriptID, err := h.Signer.Verify(request.JobToken, userID, h.now())
	if err != nil {
		status := http.StatusForbidden
		if errors.Is(err, assemblyaiservice.ErrExpiredJobToken) {
			status = http.StatusGone
		}
		h.writeError(r, status, "INVALID_JOB_TOKEN", err.Error())
		return
	}
	result, err := h.Service.Query(r.Context(), transcriptID)
	if err != nil {
		h.writeError(r, http.StatusBadGateway, "ASSEMBLYAI_QUERY_FAILED", err.Error())
		return
	}
	h.write(r, http.StatusOK, map[string]any{"code": 0, "message": "success", "data": result})
}

func (h SpeakerIdentificationHandlers) authenticate(r *ghttp.Request) (string, bool) {
	if h.AuthenticateUser == nil {
		h.writeError(r, http.StatusServiceUnavailable, "AUTH_UNAVAILABLE", "authentication is not configured")
		return "", false
	}
	userID, err := h.AuthenticateUser(r)
	if err != nil || strings.TrimSpace(userID) == "" {
		h.writeError(r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return "", false
	}
	return userID, true
}

func (h SpeakerIdentificationHandlers) writeError(r *ghttp.Request, status int, code, message string) {
	h.write(r, status, map[string]any{"code": code, "message": message, "data": nil})
}

func (h SpeakerIdentificationHandlers) write(r *ghttp.Request, status int, value any) {
	r.Response.WriteHeader(status)
	r.Response.Header().Set("Content-Type", "application/json")
	r.Response.WriteJson(value)
}

func (h SpeakerIdentificationHandlers) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func validateIdentificationConfig(speakerType string, raw []struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}) (assemblyaiservice.IdentificationConfig, error) {
	if speakerType != "name" && speakerType != "role" {
		return assemblyaiservice.IdentificationConfig{}, errors.New("speaker_type must be name or role")
	}
	if len(raw) == 0 || len(raw) > maxCandidates {
		return assemblyaiservice.IdentificationConfig{}, errors.New("speakers must contain between 1 and 10 candidates")
	}
	seen := make(map[string]struct{}, len(raw))
	speakers := make([]assemblyaiservice.SpeakerCandidate, 0, len(raw))
	for _, item := range raw {
		value := strings.TrimSpace(item.Value)
		description := strings.TrimSpace(item.Description)
		key := strings.ToLower(value)
		if value == "" || utf8.RuneCountInString(value) > maxCandidateValueRunes || utf8.RuneCountInString(description) > maxDescriptionRunes {
			return assemblyaiservice.IdentificationConfig{}, errors.New("speaker candidate is blank or too long")
		}
		if _, exists := seen[key]; exists {
			return assemblyaiservice.IdentificationConfig{}, errors.New("speaker candidates must be unique")
		}
		seen[key] = struct{}{}
		speakers = append(speakers, assemblyaiservice.SpeakerCandidate{Value: value, Description: description})
	}
	return assemblyaiservice.IdentificationConfig{SpeakerType: speakerType, Speakers: speakers}, nil
}
