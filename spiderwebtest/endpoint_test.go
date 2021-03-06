package spiderwebtest

import (
	"testing"

	"github.com/wspowell/spiderweb/examples/app"
)

func Test_EndpointTest(t *testing.T) {
	t.Parallel()

	// Request should not be altered.
	requestBody := &app.MyRequestBodyModel{
		MyInt:      5,
		MyString:   "hello",
		ShouldFail: false,
	}

	postResource := &app.PostResource{
		Test:         "",
		RequestBody:  requestBody,
		ResponseBody: &app.MyResponseBodyModel{},
	}

	expectedHttpStatus := 201
	var expectedErr error
	expectedPostResource := &app.PostResource{
		Test:        "",
		RequestBody: requestBody,
		ResponseBody: &app.MyResponseBodyModel{
			MyInt:    5,
			MyString: "hello",
		},
	}

	TestEndpoint(t, postResource, expectedPostResource, expectedHttpStatus, expectedErr)
}
