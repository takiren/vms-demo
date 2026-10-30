package validation_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
	"github.com/takiren/vms-demo/server/internal/api/gen"
	"github.com/takiren/vms-demo/server/internal/api/validation"
)

const testSpec = `
openapi: 3.0.3
info: {title: test, version: "1"}
servers:
  - url: /api
paths:
  /items/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: string, format: uuid}}
    get:
      operationId: getItem
      parameters:
        - {name: limit, in: query, schema: {type: integer, minimum: 1, maximum: 100}}
      responses:
        "200": {description: ok}
  /items:
    post:
      operationId: createItem
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              additionalProperties: false
              required: [name]
              properties:
                name: {type: string, minLength: 1}
      responses:
        "201": {description: created}
    get:
      operationId: listItems
      responses:
        "200": {description: ok}
`

const validID = "01890a5d-ac96-774b-bcce-b302099a8057"

func newServer(t *testing.T) *echo.Echo {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData([]byte(testSpec))
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	mw, err := validation.Middleware(doc)
	if err != nil {
		t.Fatalf("Middleware: %v", err)
	}
	logger := slog.New(slog.DiscardHandler)
	e := echo.NewWithConfig(echo.Config{Logger: logger, HTTPErrorHandler: apierror.Handler(logger)})
	e.Use(middleware.BodyLimit(64), mw)
	ok := func(c *echo.Context) error { return c.NoContent(http.StatusOK) }
	e.GET("/api/items/:id", ok)
	e.GET("/api/items", ok)
	e.GET("/api/unspecified", ok)
	e.POST("/api/items", func(c *echo.Context) error {
		// The validated body must still be readable by the handler.
		b, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		return c.Blob(http.StatusCreated, "application/json", b)
	})
	return e
}

func do(e *echo.Echo, method, target, contentType, body string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestValidRequestsPass(t *testing.T) {
	e := newServer(t)
	tests := []struct {
		name, method, target, ct, body string
		want                           int
	}{
		{"path param", http.MethodGet, "/api/items/" + validID, "", "", 200},
		{"query param", http.MethodGet, "/api/items/" + validID + "?limit=10", "", "", 200},
		{"no params", http.MethodGet, "/api/items", "", "", 200},
		{"json body", http.MethodPost, "/api/items", "application/json", `{"name":"cam"}`, 201},
		{"json body with charset", http.MethodPost, "/api/items", "application/json; charset=utf-8", `{"name":"cam"}`, 201},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(e, tt.method, tt.target, tt.ct, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body.String())
			}
			if tt.body != "" && strings.TrimSpace(rec.Body.String()) != tt.body {
				t.Errorf("handler saw body %q, want %q", rec.Body.String(), tt.body)
			}
		})
	}
}

func TestInvalidRequestsAreRejected(t *testing.T) {
	e := newServer(t)
	tests := []struct {
		name, method, target, ct, body string
		wantStatus                     int
		wantCode                       gen.ErrorCode
		wantMsg                        string
	}{
		{"bad path param", http.MethodGet, "/api/items/not-a-uuid", "", "", 400, gen.ErrorCodeBadRequest, `path parameter "id"`},
		{"query out of range", http.MethodGet, "/api/items/" + validID + "?limit=1000", "", "", 400, gen.ErrorCodeBadRequest, `query parameter "limit"`},
		{"query not integer", http.MethodGet, "/api/items/" + validID + "?limit=abc", "", "", 400, gen.ErrorCodeBadRequest, `query parameter "limit": an invalid integer`},
		{"missing body", http.MethodPost, "/api/items", "application/json", "", 400, gen.ErrorCodeBadRequest, "request body: is required"},
		{"missing required property", http.MethodPost, "/api/items", "application/json", `{}`, 400, gen.ErrorCodeBadRequest, "request body"},
		{"unknown property", http.MethodPost, "/api/items", "application/json", `{"name":"a","admin":true}`, 400, gen.ErrorCodeBadRequest, "request body"},
		{"wrong type", http.MethodPost, "/api/items", "application/json", `{"name":1}`, 400, gen.ErrorCodeBadRequest, "/name"},
		{"malformed json", http.MethodPost, "/api/items", "application/json", `{"name":`, 400, gen.ErrorCodeBadRequest, "request body"},
		{"wrong content type", http.MethodPost, "/api/items", "text/plain", `name=a`, 415, gen.ErrorCodeUnsupportedMediaType, "unsupported content type"},
		{"body not in spec", http.MethodGet, "/api/items", "application/json", `{"a":1}`, 400, gen.ErrorCodeBadRequest, "request body"},
		{"body too large", http.MethodPost, "/api/items", "application/json", `{"name":"` + strings.Repeat("a", 100) + `"}`, 413, gen.ErrorCodePayloadTooLarge, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(e, tt.method, tt.target, tt.ct, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var body gen.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not the common error shape: %q: %v", rec.Body.String(), err)
			}
			if body.Error.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.wantCode)
			}
			if !strings.Contains(body.Error.Message, tt.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", body.Error.Message, tt.wantMsg)
			}
		})
	}
}

func TestUnknownRoutesPassThrough(t *testing.T) {
	e := newServer(t)
	// Registered in Echo but not in the spec: not validated.
	if rec := do(e, http.MethodGet, "/api/unspecified?x=1", "", ""); rec.Code != http.StatusOK {
		t.Errorf("unspecified route: status = %d", rec.Code)
	}
	// Unknown to both: Echo's 404 in the common format.
	rec := do(e, http.MethodGet, "/api/nope", "", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Errorf("unknown route: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMiddlewareRejectsInvalidSpec(t *testing.T) {
	doc := &openapi3.T{OpenAPI: "3.0.3"} // missing info and paths
	if _, err := validation.Middleware(doc); err == nil {
		t.Error("Middleware accepted an invalid document")
	}
}
