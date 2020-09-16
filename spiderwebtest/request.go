package spiderwebtest

import (
	"bytes"
	"testing"

	"github.com/wspowell/spiderweb"

	"github.com/valyala/fasthttp"
)

type requestTestCase struct {
	httpMethod  string
	path        string
	requestBody []byte

	httpStatus   int
	responseBody []byte
}

// GivenRequest starts a request test case to be provided to TestRequest.
func GivenRequest(httpMethod string, path string) *requestTestCase {
	return &requestTestCase{
		httpMethod: httpMethod,
		path:       path,
	}
}

// WithRequestBody sets a request body for the request test case.
// This is optional.
func (self *requestTestCase) WithRequestBody(requestBody []byte) *requestTestCase {
	self.requestBody = requestBody
	return self
}

// Expect the response to match the given status and body.
func (self *requestTestCase) Expect(httpStatus int, responseBody []byte) *requestTestCase {
	self.httpStatus = httpStatus
	self.responseBody = responseBody
	return self
}

// TestRequest for request/response roundtrip.
func TestRequest(t *testing.T, server spiderweb.Server, testCase *requestTestCase) {
	copyRequestBody := make([]byte, len(testCase.requestBody))
	copyResponseBody := make([]byte, len(testCase.responseBody))

	copy(copyRequestBody, testCase.requestBody)
	copy(copyResponseBody, testCase.responseBody)

	copyRequestTestCase := requestTestCase{
		httpMethod:   testCase.httpMethod,
		path:         testCase.path,
		requestBody:  copyRequestBody,
		httpStatus:   testCase.httpStatus,
		responseBody: copyResponseBody,
	}

	var req fasthttp.Request

	req.Header.SetMethod(copyRequestTestCase.httpMethod)
	req.Header.SetRequestURI(copyRequestTestCase.path)
	req.Header.Set(fasthttp.HeaderHost, "localhost")
	req.SetBody(copyRequestTestCase.requestBody)

	requestCtx := fasthttp.RequestCtx{}
	requestCtx.Init(&req, nil, nil)

	actualHttpStatus, actualResponseBody := server.Execute(&requestCtx)

	if copyRequestTestCase.httpStatus != actualHttpStatus {
		t.Errorf("expected http status %v, but got %v", copyRequestTestCase.httpStatus, actualHttpStatus)
	}

	if !bytes.Equal(copyRequestTestCase.responseBody, actualResponseBody) {
		t.Errorf("expected request body '%v', but got '%v'", string(copyRequestTestCase.responseBody), string(actualResponseBody))
	}
}