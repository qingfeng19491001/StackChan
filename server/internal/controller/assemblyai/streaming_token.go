package assemblyai

import (
	"errors"
	"stackChan/internal/assemblyai"

	"github.com/gogf/gf/v2/net/ghttp"
)

const (
	defaultExpiresIn          = 60
	defaultMaxSessionDuration = 10800
)

// StreamingToken returns a short-lived, single-use AssemblyAI token.
// The permanent AssemblyAI API key is kept server-side by the service.
func StreamingToken(r *ghttp.Request) {
	expiresIn := r.GetQuery("expires_in_seconds", defaultExpiresIn).Int()
	maxSessionDuration := r.GetQuery("max_session_duration_seconds", defaultMaxSessionDuration).Int()

	token, err := assemblyai.NewService().CreateStreamingToken(
		r.Context(), expiresIn, maxSessionDuration,
	)
	if err != nil {
		status := 502
		if errors.Is(err, assemblyai.ErrAPIKeyNotConfigured) {
			status = 503
		}
		r.Response.WriteStatus(status)
		r.Response.WriteJson(map[string]any{
			"code":    status,
			"message": err.Error(),
			"data":    nil,
		})
		return
	}

	r.Response.WriteJson(map[string]any{
		"code":    0,
		"message": "success",
		"data":    token,
	})
}
