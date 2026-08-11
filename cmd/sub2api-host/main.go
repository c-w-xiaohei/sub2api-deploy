package main

import (
	"context"
	"io"
	"os"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "stdio" && os.Args[1] != "bootstrap-stdio") {
		os.Exit(2)
	}
	runtime := hostruntime.New("", "")
	var err error
	if os.Args[1] == "bootstrap-stdio" {
		err = bootstrapServe(os.Stdout, os.Stdin, runtime)
	} else {
		err = serve(os.Stdout, os.Stdin, runtime)
	}
	if err != nil {
		os.Exit(1)
	}
}

func serve(out io.Writer, in io.Reader, runtime *hostruntime.Runtime) error {
	return serveRequest(out, in, runtime.Handle)
}

func bootstrapServe(out io.Writer, in io.Reader, runtime *hostruntime.Runtime) error {
	return serveRequest(out, in, runtime.Bootstrap)
}

func serveRequest(out io.Writer, in io.Reader, handle func(context.Context, hostprotocol.Request) (hostprotocol.Result, error)) error {
	request, err := hostprotocol.DecodeRequestFrom(in)
	if err != nil {
		return writeResponse(out, hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}}, err)
	}
	result, operationErr := handle(context.Background(), request)
	response := hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorRemoteOperation, Code: hostprotocol.CodeOperationFailed}}
	if operationErr == nil {
		response = hostprotocol.Response{Result: &result}
	} else if remote, ok := operationErr.(*hostruntime.RemoteError); ok {
		response = hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: remote.Category, Code: remote.Code}}
	}
	return writeResponse(out, response, nil)
}

// writeResponse is the sole stdout path so every accepted invocation has one frame.
func writeResponse(out io.Writer, response hostprotocol.Response, requestErr error) error {
	frame, err := hostprotocol.EncodeResponse(response)
	if err != nil {
		return err
	}
	if _, err = out.Write(frame); err != nil {
		return err
	}
	return requestErr
}
