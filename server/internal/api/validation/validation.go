// Package validation validates requests against the OpenAPI document before
// they reach handlers.
//
// oapi-codegen's echo-middleware only supports Echo v4, so this is a thin
// adapter over kin-openapi's openapi3filter for Echo v5.
package validation

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api/apierror"
)

// Middleware returns an Echo middleware that validates each request's path,
// query, header and cookie parameters and its body against doc.
//
// Requests that do not match any operation in doc are passed through
// unchanged, so Echo's router answers them with 404 or 405.
//
// Authentication is not checked here (security requirements are ignored);
// it is the job of the authentication middleware.
//
// Invalid requests are rejected with an *apierror.Error (400, 413 or 415).
func Middleware(doc *openapi3.T) (echo.MiddlewareFunc, error) {
	if err := doc.Validate(openapi3.NewLoader().Context); err != nil {
		return nil, fmt.Errorf("validation: invalid OpenAPI document: %w", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("validation: build router: %w", err)
	}
	opts := &openapi3filter.Options{
		AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
		// Hand the validated body to the generated strict handler, which
		// binds it instead of decoding the body a second time.
		KeepValidatedValue:                true,
		RejectWhenRequestBodyNotSpecified: true,
		// kin-openapi checks only a few formats by default. IDs are UUIDv7,
		// so accept RFC 9562 UUIDs.
		SchemaValidationOptions: []openapi3.SchemaValidationOption{
			openapi3.WithStringFormatValidators(map[string]openapi3.StringFormatValidator{
				"uuid": openapi3.NewRegexpFormatValidator(openapi3.FormatOfStringForUUIDOfRFC9562),
			}),
		},
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req := c.Request()
			route, pathParams, err := router.FindRoute(req)
			if err != nil {
				// Unknown path or method: let Echo's router respond.
				return next(c)
			}
			input := &openapi3filter.RequestValidationInput{
				Request:    req,
				PathParams: pathParams,
				Route:      route,
				Options:    opts,
			}
			if err := openapi3filter.ValidateRequest(req.Context(), input); err != nil {
				return toAPIError(err)
			}
			return next(c)
		}
	}, nil
}

// toAPIError converts a validation failure into a client-facing error. The
// message names what is wrong (parameter or body location and the schema
// rule) but does not echo the rejected value back.
func toAPIError(err error) error {
	// Errors that already carry a status, such as the body limit's 413 while
	// the body is being read, win over the generic 400.
	if s := echo.StatusCode(err); s != 0 {
		return apierror.Wrap(s, http.StatusText(s), err)
	}

	var reqErr *openapi3filter.RequestError
	if !errors.As(err, &reqErr) {
		return apierror.Wrap(http.StatusBadRequest, "invalid request", err)
	}

	if strings.HasPrefix(reqErr.Reason, "unsupported content type") ||
		strings.HasPrefix(reqErr.Reason, "header Content-Type has unexpected value") {
		return apierror.Wrap(http.StatusUnsupportedMediaType, "unsupported content type", err)
	}

	var where string
	reason := reqErr.Reason
	switch {
	case reqErr.Parameter != nil:
		where = fmt.Sprintf("%s parameter %q", reqErr.Parameter.In, reqErr.Parameter.Name)
	case reqErr.RequestBody != nil:
		where = "request body"
	default:
		// Errors not tied to a parameter or the body schema, such as a body
		// sent to an operation that takes none. kin-openapi's message does
		// not include request values here.
		where = "request"
		if reason == "" && reqErr.Err != nil {
			reason = reqErr.Err.Error()
		}
	}

	var schemaErr *openapi3.SchemaError
	var parseErr *openapi3filter.ParseError
	switch {
	case errors.As(reqErr.Err, &schemaErr):
		reason = schemaErr.Reason
		if ptr := schemaErr.JSONPointer(); len(ptr) > 0 {
			reason = fmt.Sprintf("/%s: %s", strings.Join(ptr, "/"), reason)
		}
	case errors.Is(reqErr.Err, openapi3filter.ErrInvalidRequired):
		reason = "is required"
	case errors.Is(reqErr.Err, openapi3filter.ErrInvalidEmptyValue):
		reason = "must not be empty"
	case errors.As(reqErr.Err, &parseErr) && parseErr.Reason != "":
		// ParseError.Error() includes the rejected value; Reason does not.
		reason = parseErr.Reason
	}
	if reason == "" {
		reason = "is invalid"
	}
	return apierror.Wrap(http.StatusBadRequest, where+": "+reason, err)
}
