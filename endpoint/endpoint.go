package endpoint

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/wspowell/errors"
	"github.com/wspowell/logging"
	"github.com/wspowell/spiderweb/profiling"
)

const (
	structTagKey = "spiderweb"

	structTagValueRequest  = "request"
	structTagValueResponse = "response"

	structTagOptionValidate = "validate"
)

const (
	HeaderAccept = "Accept"
)

var (
	nullBytes = []byte("null")
)

// Config defines the behavior of an endpoint.
// Endpoint behavior is interface driven and can be completely modified by an application.
// The values in the config must never be modified by an endpoint.
type Config struct {
	LogConfig         logging.Configer
	ErrorHandler      ErrorHandler
	Auther            Auther
	RequestValidator  RequestValidator
	ResponseValidator ResponseValidator
	MimeTypeHandlers  MimeTypeHandlers
	Resources         map[string]interface{}
	Timeout           time.Duration
}

// Endpoint defines the behavior of a given handler.
type Endpoint struct {
	Config *Config

	handlerData handlerTypeData
}

// Create a new endpoint that will run the given handler.
// This will be created by the Server during normal operations.
func NewEndpoint(config *Config, handler Handler) *Endpoint {
	configClone := &Config{}

	// Set defaults, if not set.

	if config.ErrorHandler == nil {
		configClone.ErrorHandler = defaultErrorHandler{}
	} else {
		configClone.ErrorHandler = config.ErrorHandler
	}

	if config.MimeTypeHandlers == nil {
		configClone.MimeTypeHandlers = NewMimeTypeHandlers()
	} else {
		configClone.MimeTypeHandlers = config.MimeTypeHandlers
	}

	if config.Resources == nil {
		configClone.Resources = map[string]interface{}{}
	} else {
		configClone.Resources = config.Resources
	}

	if config.Timeout == 0 {
		configClone.Timeout = 30 * time.Second
	} else {
		configClone.Timeout = config.Timeout
	}

	return &Endpoint{
		Config: configClone,

		handlerData: newHandlerTypeData(handler),
	}
}

func (self *Endpoint) Name() string {
	return self.handlerData.structName
}

// Execute the endpoint and run the endpoint handler.
func (self *Endpoint) Execute(ctx *Context) (httpStatus int, responseBody []byte) {
	var responseMimeType *MimeTypeHandler

	defer func() {
		if errPanic := recover(); errPanic != nil {
			logging.Error(ctx, "panic: %+v", errors.New("ERROR", "%+v", errPanic))
			httpStatus, responseBody = self.processErrorResponse(ctx, responseMimeType, http.StatusInternalServerError, ErrorPanic)
		}
	}()

	defer profiling.Profile(ctx, string(ctx.HttpMethod)+" "+ctx.MatchedPath).Finish()

	// Setup logging.
	{
		// Every invocation of an endpoint is guaranteed to get its own logger instance.
		// See: logging.WithContext()
		logging.Tag(ctx, "request_id", ctx.requestCtx.ID())
		logging.Tag(ctx, "method", string(ctx.requestCtx.Method()))
		logging.Tag(ctx, "route", ctx.MatchedPath)
		logging.Tag(ctx, "path", string(ctx.requestCtx.URI().Path()))
		logging.Tag(ctx, "action", self.Name())

		// Each path parameter is added as a log tag.
		// Note: It helps if the path parameter name is descriptive.
		for param := range self.handlerData.pathParameters {
			if value, ok := ctx.requestCtx.UserValue(param).(string); ok {
				logging.Tag(ctx, param, value)
			}
		}
	}

	logging.Trace(ctx, "executing endpoint")

	var err error

	// Content-Type and Accept
	var requestMimeType *MimeTypeHandler
	{
		var ok bool

		if self.handlerData.hasRequestBody {
			logging.Trace(ctx, "processing request body mime type")

			contentType := ctx.Request().Header.ContentType()
			if len(contentType) == 0 {
				logging.Debug(ctx, "header Content-Type not found")
				return self.processErrorResponse(ctx, responseMimeType, http.StatusUnsupportedMediaType, errors.New(InternalCodeRequestMimeTypeMissing, "Content-Type MIME type not provided"))
			}

			requestMimeType, ok = self.Config.MimeTypeHandlers.Get(contentType, self.handlerData.requestMimeTypes)
			if !ok {
				logging.Debug(ctx, "mime type handler not available: %s", contentType)
				return self.processErrorResponse(ctx, responseMimeType, http.StatusUnsupportedMediaType, errors.New(InternalCodeRequestMimeTypeUnsupported, "Content-Type MIME type not supported: %s", contentType))
			}

			logging.Debug(ctx, "found request mime type handler: %s", contentType)
		}

		if self.handlerData.hasResponseBody {
			logging.Trace(ctx, "processing response body mime type")

			accept := ctx.Request().Header.Peek(HeaderAccept)
			if len(accept) == 0 {
				logging.Debug(ctx, "header Accept not found")
				return self.processErrorResponse(ctx, responseMimeType, http.StatusUnsupportedMediaType, errors.New(InternalCodeResponseMimeTypeMissing, "Accept MIME type not provided"))
			}

			responseMimeType, ok = self.Config.MimeTypeHandlers.Get(accept, self.handlerData.responseMimeTypes)
			if !ok {
				logging.Debug(ctx, "mime type handler not available: %s", accept)
				return self.processErrorResponse(ctx, responseMimeType, http.StatusUnsupportedMediaType, errors.New(InternalCodeResponseMimeTypeUnsupported, "Accept MIME type not supported: %s", accept))
			}
			// All responses after this must be marshalable to the mime type.
			ctx.requestCtx.SetContentType(responseMimeType.MimeType)

			logging.Debug(ctx, "found response mime type handler: %s", accept)
		}
	}

	if !ctx.ShouldContinue() {
		logging.Debug(ctx, "request canceled or timed out")
		return self.processErrorResponse(ctx, responseMimeType, http.StatusRequestTimeout, ErrorRequestTimeout)
	}

	// Authentication
	{
		authTimer := profiling.Profile(ctx, "Auth")

		if self.Config.Auther != nil {
			logging.Trace(ctx, "processing auth handler")

			httpStatus, err = self.Config.Auther.Auth(ctx, ctx.Request().Header.VisitAll)
			authTimer.Finish()
			if err != nil {
				logging.Debug(ctx, "auth failed")
				return self.processErrorResponse(ctx, responseMimeType, httpStatus, err)
			}
		}
	}

	if !ctx.ShouldContinue() {
		logging.Debug(ctx, "request canceled or timed out")
		return self.processErrorResponse(ctx, responseMimeType, http.StatusRequestTimeout, ErrorRequestTimeout)
	}

	logging.Trace(ctx, "allocating handler")

	allocateTimer := profiling.Profile(ctx, "Allocate")
	handlerAlloc := self.handlerData.allocateHandler()

	self.handlerData.setResources(handlerAlloc.handlerValue, self.Config.Resources)
	self.handlerData.setPathParameters(handlerAlloc.handlerValue, ctx.requestCtx)
	self.handlerData.setQueryParameters(handlerAlloc.handlerValue, ctx.requestCtx)
	allocateTimer.Finish()

	// Handle Request
	{
		if !ctx.ShouldContinue() {
			logging.Debug(ctx, "request canceled or timed out")
			return self.processErrorResponse(ctx, responseMimeType, http.StatusRequestTimeout, ErrorRequestTimeout)
		}

		if self.handlerData.hasRequestBody {
			logging.Trace(ctx, "processing request body")

			requestBodyBytes := ctx.Request().Body()

			populateRequestTimer := profiling.Profile(ctx, "UnmarshalRequest")
			err = self.setHandlerRequestBody(ctx, requestMimeType, handlerAlloc.requestBody, requestBodyBytes)
			populateRequestTimer.Finish()
			if err != nil {
				logging.Debug(ctx, "failed processing request body")
				return self.processErrorResponse(ctx, responseMimeType, http.StatusBadRequest, err)
			}

			if self.Config.RequestValidator != nil && self.handlerData.shouldValidateRequest {
				logging.Trace(ctx, "processing validation handler")

				validateTimer := profiling.Profile(ctx, "ValidateRequest")
				var validationFailure error
				httpStatus, validationFailure = self.Config.RequestValidator.ValidateRequest(ctx, requestBodyBytes)
				validateTimer.Finish()
				if validationFailure != nil {
					logging.Debug(ctx, "failed request body validation")

					// Validation failures are not hard errors and should be passed through to the error handler.
					// The failure is passed through since it is assumed this error contains information to be returned in the response.
					return self.processErrorResponse(ctx, responseMimeType, httpStatus, validationFailure)
				}
			}
		}
	}

	if !ctx.ShouldContinue() {
		logging.Debug(ctx, "request canceled or timed out")
		return self.processErrorResponse(ctx, responseMimeType, http.StatusRequestTimeout, ErrorRequestTimeout)
	}

	// Run the endpoint handler.
	logging.Trace(ctx, "running endpoint handler")
	handleTimer := profiling.Profile(ctx, self.Name()+".Handle()")
	httpStatus, err = handlerAlloc.handler.Handle(ctx)
	handleTimer.Finish()
	if err != nil {
		logging.Debug(ctx, "handler error")
		return self.processErrorResponse(ctx, responseMimeType, httpStatus, err)
	}

	// Handle Response
	{
		if !ctx.ShouldContinue() {
			logging.Debug(ctx, "request canceled or timed out")
			return self.processErrorResponse(ctx, responseMimeType, http.StatusRequestTimeout, ErrorRequestTimeout)
		}

		populateResponseTimer := profiling.Profile(ctx, "MarshalResponseBody")
		responseBody, err = self.getHandlerResponseBody(ctx, responseMimeType, handlerAlloc.responseBody)
		populateResponseTimer.Finish()
		if err != nil {
			logging.Debug(ctx, "failed processing response")
			return self.processErrorResponse(ctx, responseMimeType, http.StatusInternalServerError, err)
		}

		if self.Config.ResponseValidator != nil && self.handlerData.shouldValidateResponse {
			logging.Trace(ctx, "processing response validation handler")

			validateResponseTimer := profiling.Profile(ctx, "ValidateResponse")
			var validationFailure error
			httpStatus, validationFailure = self.Config.ResponseValidator.ValidateResponse(ctx, httpStatus, responseBody)
			validateResponseTimer.Finish()
			if err != nil {
				logging.Debug(ctx, "failed response validation")
				// Validation failures are not hard errors and should be passed through to the error handler.
				// The failure is passed through since it is assumed this error contains information to be returned in the response.
				return self.processErrorResponse(ctx, responseMimeType, httpStatus, validationFailure)
			}
		}
	}

	logging.Debug(ctx, "success response: %d %s", httpStatus, responseBody)

	return httpStatus, responseBody
}

func (self *Endpoint) processErrorResponse(ctx *Context, responseMimeType *MimeTypeHandler, httpStatus int, err error) (int, []byte) {
	var responseBody []byte
	var errStruct interface{}

	defer func() {
		// Print the actual error response returned to the caller.
		logging.Debug(ctx, "error response: %d %s", httpStatus, responseBody)
	}()

	if httpStatus >= 500 {
		if httpStatus == 500 {
			logging.Error(ctx, "failure (500): %+v", err)
		} else {
			logging.Error(ctx, "failure (%d): %#v", httpStatus, err)
		}
	} else {
		logging.Debug(ctx, "error (%d): %#v", httpStatus, err)
	}

	if responseMimeType == nil {
		ctx.requestCtx.SetContentType(mimeTypeTextPlain)
		responseBody = []byte(fmt.Sprintf("%#v", err))
		return httpStatus, responseBody
	}

	httpStatus, errStruct = self.Config.ErrorHandler.HandleError(ctx, httpStatus, err)
	responseBody, err = responseMimeType.Marshal(errStruct)
	if err != nil {
		ctx.requestCtx.SetContentType(mimeTypeTextPlain)
		err = errors.New(InternalCodeErrorParseFailure, "Internal server error")
		httpStatus = http.StatusInternalServerError
		responseBody = []byte(fmt.Sprintf("%s", err))
		return httpStatus, responseBody
	}

	return httpStatus, responseBody
}

func (self *Endpoint) setHandlerRequestBody(ctx *Context, mimeHandler *MimeTypeHandler, requestBody interface{}, requestBodyBytes []byte) error {
	if requestBody != nil {
		logging.Trace(ctx, "non-empty request body")

		if err := mimeHandler.Unmarshal(requestBodyBytes, requestBody); err != nil {
			logging.Error(ctx, "failed to unmarshal request body: %v", err)
			return ErrorRequestBodyUnmarshalFailure
		}
	}
	return nil
}

func (self *Endpoint) getHandlerResponseBody(ctx *Context, mimeHandler *MimeTypeHandler, responseBody interface{}) ([]byte, error) {
	if responseBody != nil {
		logging.Trace(ctx, "non-empty response body")

		ctx.requestCtx.SetContentType(mimeHandler.MimeType)
		responseBodyBytes, err := mimeHandler.Marshal(responseBody)
		if err != nil {
			logging.Error(ctx, "failed to marshal response: %v", err)
			return nil, ErrorResponseBodyMarshalFailure
		}
		if len(responseBodyBytes) == 4 && bytes.Equal(responseBodyBytes, nullBytes) {
			logging.Debug(ctx, "request body is null")
			return nil, ErrorResponseBodyNull
		}
		return responseBodyBytes, nil
	}

	// No response body.
	return nil, nil
}
