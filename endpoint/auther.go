package endpoint

import (
	"github.com/valyala/fasthttp"
)

// Auther defines request authentication.
type Auther interface {
	// TODO: #11 Pass in copies of the headers here instead of *fasthttp.Request.
	//       Consumers should not have to import fasthttp just for this.
	Auth(request *fasthttp.Request) (int, error)
}
