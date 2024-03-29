package endpoint

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/wspowell/context"
	"github.com/wspowell/errors"
	"github.com/wspowell/log"

	"github.com/wspowell/spiderweb/httpstatus"
)

const (
	structTagKey = "spiderweb"

	structTagValueRequest  = "request"
	structTagValueResponse = "response"

	structTagOptionValidate = "validate"

	structTagAuth = "auth"
)

const (
	null = "null"
)

func nullBytes() []byte {
	return []byte(null)
}

// Config defines the behavior of an endpoint.
// Endpoint behavior is interface driven and can be completely modified by an application.
// The values in the config must never be modified by an endpoint.
type Config struct {
	LogConfig         log.LoggerConfig
	ErrorHandler      ErrorHandler
	RequestValidator  RequestValidator
	ResponseValidator ResponseValidator
	MimeTypeHandlers  MimeTypeHandlers
	Resources         map[string]any
	Timeout           time.Duration
	Tracer            opentracing.Tracer
}

// Endpoint defines the behavior of a given handler.
type Endpoint struct {
	Config *Config

	handlerData handlerTypeData
}

// Create a new endpoint that will run the given handler.
// This will be created by the Server during normal operations.
func NewEndpoint(ctx context.Context, config *Config, handler Handler) *Endpoint {
	configClone := &Config{}

	// Set defaults, if not set.

	if config.LogConfig == nil {
		configClone.LogConfig = log.NewConfig()
	} else {
		configClone.LogConfig = config.LogConfig
	}

	if config.ErrorHandler == nil {
		configClone.ErrorHandler = defaultErrorHandler{}
	} else {
		configClone.ErrorHandler = config.ErrorHandler
	}

	if config.MimeTypeHandlers == nil {
		configClone.MimeTypeHandlers = NewMimeTypeHandlers()
	} else {
		// Use known handlers as a base for mime handlers.
		// This enables all endpoints to use the defaults
		//   even if no request or response body is defined.
		mimeHandlers := NewMimeTypeHandlers()
		for mime, handler := range config.MimeTypeHandlers {
			mimeHandlers[mime] = handler
		}
		configClone.MimeTypeHandlers = mimeHandlers
	}

	if config.Resources == nil {
		configClone.Resources = map[string]any{}
	} else {
		configClone.Resources = config.Resources
	}

	if config.Timeout == 0 {
		configClone.Timeout = 30 * time.Second
	} else {
		configClone.Timeout = config.Timeout
	}

	if config.Tracer == nil {
		configClone.Tracer = opentracing.GlobalTracer()
	} else {
		configClone.Tracer = config.Tracer
	}

	return &Endpoint{
		Config: configClone,

		handlerData: newHandlerTypeData(ctx, handler),
	}
}

func (self *Endpoint) Name() string {
	return self.handlerData.structName
}

// Execute the endpoint and run the endpoint handler.
func (self *Endpoint) Execute(ctx context.Context, requester Requester) (httpStatus int, responseBody []byte) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "Execute()")
	defer span.Finish()

	ctx = context.Localize(ctx)

	// Every invocation of an endpoint creates its own logger instance.
	ctx = log.WithContext(ctx, self.Config.LogConfig)

	var responseMimeType *MimeTypeHandler

	// Defer recover at this point so that logging and context has been initialized.
	defer func() {
		if err := errors.Recover(recover()); err != nil {
			log.Error(ctx, "panic: %+v", err)
			// Convert the panic error to an internal server error. Never expose panics directly.
			err = errors.Wrap(err, ErrInternalServerError)
			httpStatus, responseBody = self.processErrorResponse(ctx, requester, responseMimeType, http.StatusInternalServerError, err)
		}
	}()

	requester.SetResponseHeader("X-Request-Id", requester.RequestId())

	// Setup log.
	{
		logSpan, ctx := opentracing.StartSpanFromContext(ctx, "setup log")

		log.Tag(ctx, "request_id", requester.RequestId())
		log.Tag(ctx, "method", string(requester.Method()))
		log.Tag(ctx, "route", requester.MatchedPath())
		log.Tag(ctx, "path", string(requester.Path()))
		log.Tag(ctx, "action", self.Name())

		// Each path parameter is added as a log tag.
		// Note: It helps if the path parameter name is descriptive.
		for param := range self.handlerData.pathParameters {
			if value, ok := requester.PathParam(param); ok {
				log.Tag(ctx, param, value)
			}
		}

		logSpan.Finish()
	}

	log.Trace(ctx, "executing endpoint")

	var err error

	// Content-Type and Accept
	var requestMimeType *MimeTypeHandler
	{
		mimeTypeSpan, ctx := opentracing.StartSpanFromContext(ctx, "setup mime types")

		var ok bool

		if self.handlerData.hasRequestBody {
			log.Trace(ctx, "processing request body mime type")

			contentType := requester.ContentType()
			if len(contentType) == 0 {
				log.Debug(ctx, "header Content-Type not found")
				mimeTypeSpan.Finish()

				return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusUnsupportedMediaType, errors.Wrap(ErrInvalidMimeType, errors.New("Content-Type MIME type not provided")))
			}

			requestMimeType, ok = self.Config.MimeTypeHandlers.Get(contentType, self.handlerData.requestMimeTypes)
			if !ok {
				log.Debug(ctx, "mime type handler not available: %s", contentType)
				mimeTypeSpan.Finish()

				return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusUnsupportedMediaType, errors.Wrap(ErrInvalidMimeType, errors.New("Content-Type MIME type not supported: %s", contentType)))
			}

			log.Trace(ctx, "found request mime type handler: %s", contentType)
		}

		// Always process the Accept header since an error may be returned even if there is no response body.
		log.Trace(ctx, "processing response body mime type")

		accept := requester.Accept()
		if len(accept) == 0 {
			log.Debug(ctx, "header Accept not found")
			mimeTypeSpan.Finish()

			return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusUnsupportedMediaType, errors.Wrap(ErrInvalidMimeType, errors.New("Accept MIME type not provided")))
		}

		responseMimeType, ok = self.Config.MimeTypeHandlers.Get(accept, self.handlerData.responseMimeTypes)
		if !ok {
			log.Debug(ctx, "mime type handler not available: %s", accept)
			mimeTypeSpan.Finish()

			return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusUnsupportedMediaType, errors.Wrap(ErrInvalidMimeType, errors.New("Accept MIME type not supported: %s", accept)))
		}
		// All responses after this must be marshalable to the mime type.
		requester.SetResponseContentType(responseMimeType.MimeType)

		log.Trace(ctx, "found response mime type handler: %s", accept)

		mimeTypeSpan.Finish()
	}

	log.Trace(ctx, "allocating handler")

	allocSpan, ctx := opentracing.StartSpanFromContext(ctx, "handler allocation")

	handlerAlloc := self.handlerData.allocateHandler()
	if err = self.handlerData.setResources(handlerAlloc.handlerValue, self.Config.Resources); err != nil {
		log.Debug(ctx, "failed to set resources")
		allocSpan.Finish()

		return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusInternalServerError, errors.Wrap(err, ErrInternalServerError))
	}
	if err = self.handlerData.setPathParameters(handlerAlloc.handlerValue, requester); err != nil {
		log.Debug(ctx, "failed to set path parameters")
		allocSpan.Finish()

		return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusBadRequest, errors.Wrap(err, ErrBadRequest))
	}
	if err = self.handlerData.setQueryParameters(handlerAlloc.handlerValue, requester); err != nil {
		log.Debug(ctx, "failed to set query parameters")
		allocSpan.Finish()

		return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusBadRequest, errors.Wrap(err, ErrBadRequest))
	}

	allocSpan.Finish()

	// Authentication
	{
		authSpan, ctx := opentracing.StartSpanFromContext(ctx, "authorization")

		if handlerAlloc.auth != nil {
			log.Trace(ctx, "processing auth handler")

			if asAuthorizer, ok := handlerAlloc.auth.(Authorizer); ok {
				httpStatus, err = asAuthorizer.Authorization(ctx, requester.PeekHeader)
				if err != nil {
					log.Debug(ctx, "authorization failed")
					authSpan.Finish()

					return self.processErrorResponse(ctx, requester, responseMimeType, httpStatus, err)
				}
			} else {
				log.Debug(ctx, "authorization object does not implement Authorizer")
				authSpan.Finish()

				return self.processErrorResponse(ctx, requester, responseMimeType, httpstatus.InternalServerError, errors.Wrap(err, ErrInternalServerError))
			}
		}

		authSpan.Finish()
	}

	// Handle Request Body
	{
		requestBodySpan, ctx := opentracing.StartSpanFromContext(ctx, "process request body")

		if self.handlerData.hasRequestBody {
			log.Trace(ctx, "processing request body")

			requestBodyBytes := requester.RequestBody()

			err = self.setHandlerRequestBody(ctx, requestMimeType, handlerAlloc.requestBody, requestBodyBytes)
			if err != nil {
				log.Debug(ctx, "failed processing request body")
				requestBodySpan.Finish()

				return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusBadRequest, err)
			}

			if self.Config.RequestValidator != nil && self.handlerData.shouldValidateRequest {
				log.Trace(ctx, "processing validation handler")

				var validationFailure error
				httpStatus, validationFailure = self.Config.RequestValidator.ValidateRequest(ctx, requestBodyBytes)
				if validationFailure != nil {
					log.Debug(ctx, "failed request body validation")

					// Validation failures are not hard errors and should be passed through to the error handler.
					// The failure is passed through since it is assumed this error contains information to be returned in the response.
					requestBodySpan.Finish()

					return self.processErrorResponse(ctx, requester, responseMimeType, httpStatus, validationFailure)
				}
			}
		}

		requestBodySpan.Finish()
	}

	if !ShouldContinue(ctx) {
		log.Debug(ctx, "request canceled or timed out")

		return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusRequestTimeout, ErrRequestTimeout)
	}

	// Run the endpoint handler.
	log.Trace(ctx, "running endpoint handler")

	handlerSpan, ctx := opentracing.StartSpanFromContext(ctx, "Handle()")
	httpStatus, err = handlerAlloc.handler.Handle(ctx)
	handlerSpan.Finish()

	log.Trace(ctx, "completed endpoint handler")
	if err != nil {
		log.Debug(ctx, "handler error")

		return self.processErrorResponse(ctx, requester, responseMimeType, httpStatus, err)
	}

	if !ShouldContinue(ctx) {
		log.Debug(ctx, "request canceled or timed out")

		return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusRequestTimeout, ErrRequestTimeout)
	}

	// Handle Response Body
	{
		responseBodySpan, ctx := opentracing.StartSpanFromContext(ctx, "process response body")

		responseBody, err = self.getHandlerResponseBody(ctx, requester, responseMimeType, handlerAlloc.responseBody)
		if err != nil {
			log.Debug(ctx, "failed processing response")
			responseBodySpan.Finish()

			return self.processErrorResponse(ctx, requester, responseMimeType, http.StatusInternalServerError, err)
		}

		if self.Config.ResponseValidator != nil && self.handlerData.shouldValidateResponse {
			log.Trace(ctx, "processing response validation handler")

			var validationFailure error
			httpStatus, validationFailure = self.Config.ResponseValidator.ValidateResponse(ctx, httpStatus, responseBody)
			if validationFailure != nil {
				log.Debug(ctx, "failed response validation")
				// Validation failures are not hard errors and should be passed through to the error handler.
				// The failure is passed through since it is assumed this error contains information to be returned in the response.
				responseBodySpan.Finish()

				return self.processErrorResponse(ctx, requester, responseMimeType, httpStatus, validationFailure)
			}
		}

		responseBodySpan.Finish()
	}

	log.Debug(ctx, "success response: %d %s", httpStatus, responseBody)

	if self.handlerData.eTagEnabled {
		log.Trace(ctx, "eTagEnabled, handling etag")

		return HandleETag(ctx, requester, self.handlerData.maxAgeSeconds, httpStatus, responseBody)
	}

	return httpStatus, responseBody
}

func (self *Endpoint) processErrorResponse(ctx context.Context, requester Requester, responseMimeType *MimeTypeHandler, httpStatus int, err error) (int, []byte) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "processErrorResponse()")
	defer span.Finish()

	var responseBody []byte
	var errStruct any

	defer func() {
		// Print the actual error response returned to the caller.
		log.Debug(ctx, "error response: %d %s", httpStatus, responseBody)
	}()

	if httpStatus >= 500 {
		if httpStatus == 500 {
			log.Error(ctx, "failure (500): %+v", err)
		} else {
			log.Error(ctx, "failure (%d): %#v", httpStatus, err)
		}
	} else {
		log.Debug(ctx, "error (%d): %#v", httpStatus, err)
	}

	if responseMimeType == nil {
		requester.SetResponseContentType(mimeTypeTextPlain)
		responseBody = []byte(fmt.Sprintf("%v", err))

		return httpStatus, responseBody
	}

	httpStatus, errStruct = self.Config.ErrorHandler.HandleError(ctx, httpStatus, err)
	responseBody, err = responseMimeType.Marshal(errStruct)
	if err != nil {
		requester.SetResponseContentType(mimeTypeTextPlain)
		err = errors.Wrap(err, ErrInternalServerError)
		httpStatus = http.StatusInternalServerError
		responseBody = []byte(fmt.Sprintf("%s", err))

		return httpStatus, responseBody
	}

	return httpStatus, responseBody
}

func (self *Endpoint) setHandlerRequestBody(ctx context.Context, mimeHandler *MimeTypeHandler, requestBody any, requestBodyBytes []byte) error {
	if requestBody != nil {
		log.Trace(ctx, "non-empty request body")

		if err := mimeHandler.Unmarshal(requestBodyBytes, requestBody); err != nil {
			log.Error(ctx, "failed to unmarshal request body: %v", err)

			return errors.Wrap(err, ErrBadRequest)
		}
	}

	return nil
}

func (self *Endpoint) getHandlerResponseBody(ctx context.Context, requester Requester, mimeHandler *MimeTypeHandler, responseBody any) ([]byte, error) {
	if responseBody != nil {
		log.Trace(ctx, "non-empty response body")

		requester.SetResponseContentType(mimeHandler.MimeType)
		responseBodyBytes, err := mimeHandler.Marshal(responseBody)
		if err != nil {
			log.Error(ctx, "failed to marshal response: %v", err)

			return nil, errors.Wrap(err, ErrInternalServerError)
		}
		if len(responseBodyBytes) == 4 && bytes.Equal(responseBodyBytes, nullBytes()) {
			log.Debug(ctx, "request body is null")

			return nil, errors.Wrap(err, ErrInternalServerError)
		}

		return responseBodyBytes, nil
	}

	// No response body.
	return nil, nil
}

// ShouldContinue returns true if the underlying request has not been canceled nor deadline exceeded.
func ShouldContinue(ctx context.Context) bool {
	err := ctx.Err()

	return !(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
