package rest

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// maxBodyBytes caps the size of a decoded JSON request body (1 MiB).
const maxBodyBytes = 1 << 20

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
	slog.Error("internal error", "error", err)
	RespondError(w, http.StatusInternalServerError, "internal server error")
}

// DecodeJSON reads a JSON request body (capped at maxBodyBytes) into v.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}
