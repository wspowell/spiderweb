package lambda

import (
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/wspowell/context"

	"github.com/wspowell/spiderweb/handler"
)

// FIXME: Should be able to handle any event, not just API Gateway.
// HandlerAPIGateway is an API Gateway Proxy Request handler function
type HandlerAPIGateway func(context.Context, events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error)

type Lambda struct {
	matchedPath string
	handle      *handler.Handle
}

func New(path string, handle *handler.Handle) *Lambda {
	return &Lambda{
		matchedPath: path,
		handle:      handle,
	}
}

func (self *Lambda) Start() {
	wrappedHandler := self.wrapLambdaHandler(self.handle.Runner())

	lambda.Start(wrappedHandler)
}

// FIXME: Should be able to execute a lambda, especially for testing.
// func (self *Lambda) Execute() (int, []byte) {
// 	self.router.Handler(fasthttpCtx)
// 	return fasthttpCtx.Response.StatusCode(), fasthttpCtx.Response.Body()
// }

// func (self *Lambda) Invoke(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {

// }

func (self *Lambda) wrapLambdaHandler(runner *handler.Runner) HandlerAPIGateway {
	return func(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
		// span, ctx := opentracing.StartSpanFromContextWithTracer(ctx, handle.Config.Tracer, request.HTTPMethod+" "+self.matchedPath)
		// defer span.Finish()

		response := events.APIGatewayProxyResponse{}
		requester := NewApiGatewayRequester(self.matchedPath, &request)

		ctx, cancel := context.WithTimeout(ctx, runner.Timeout())
		go func() {
			<-ctx.Done()
			cancel()
		}()

		httpStatus, responseBody := runner.Run(ctx, requester)

		response.Body = string(responseBody)
		response.StatusCode = httpStatus
		response.Headers = requester.responseHeaders

		return response, nil
	}
}
