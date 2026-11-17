package webui

import (
	"context"
	"net/http"
)

func contextWith(r *http.Request, ip string) context.Context {
	return context.WithValue(r.Context(), clientKey{}, ip)
}
