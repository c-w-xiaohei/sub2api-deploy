package main

import (
	"context"
	"log"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprovider"
)

var version = "0.0.0-dev"

func main() {
	if err := hostprovider.New(version).Run(context.Background(), "sub2api-host", version); err != nil {
		log.Fatal(err)
	}
}
