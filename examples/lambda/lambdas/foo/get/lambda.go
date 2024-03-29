package main

import (
	"time"

	"github.com/wspowell/log"

	"github.com/wspowell/spiderweb/endpoint"
	"github.com/wspowell/spiderweb/server/lambda"
	"github.com/wspowell/spiderweb/server/route"
)

func main() {
	config := &endpoint.Config{
		LogConfig: log.NewConfig().WithLevel(log.LevelDebug),
		Timeout:   30 * time.Second,
	}

	handler := lambda.New(config, route.Get("/foo", &get{}))
	handler.Start()
}
