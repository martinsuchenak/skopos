package rest

import (
	"encoding/json"
	"net/http"

	"github.com/paularlott/logger"
)

// maxBodyBytes caps the size of a decoded JSON request body (1 MiB).
const maxBodyBytes = 1 << 20

var appLogger logger.Logger

// SetLogger configures the package-level logger so InternalError uses the same
// logger (and format) as the rest of the application.
func SetLogger(l logger.Logger) {
	appLogger = l
}

func RespondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

func RespondError(w http.ResponseWriter, status int, message string) {
	RespondJSON(w, status, map[string]string{"error": message})
}

// InternalError logs the unexpected error and responds with a generic 500 message
// so that internal details are not leaked to clients.
func InternalError(w http.ResponseWriter, err error) {
	if appLogger != nil {
		appLogger.Error("internal error", "error", err)
	}
	RespondError(w, http.StatusInternalServerError, "internal server error")
}

// DecodeJSON reads a JSON request body (capped at maxBodyBytes) into v.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}
