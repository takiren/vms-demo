package api

import (
	"context"

	"github.com/takiren/vms-demo/server/internal/api/gen"
)

// GetHealthz reports that the process is up. It does not check
// dependencies such as the database (liveness, not readiness).
func (h *Handlers) GetHealthz(_ context.Context, _ gen.GetHealthzRequestObject) (gen.GetHealthzResponseObject, error) {
	return gen.GetHealthz200JSONResponse{Status: gen.HealthStatusOk}, nil
}
