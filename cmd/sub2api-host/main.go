package main

import (
	"context"
	"io"
	"os"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
)

const bootstrapAttestation = "sub2api-bootstrap-attested-v1"

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "stdio" && os.Args[1] != "bootstrap-stdio" && os.Args[1] != "install-attest") {
		os.Exit(2)
	}
	if os.Args[1] == "install-attest" {
		attestation := os.NewFile(3, "install-attestation")
		if attestation == nil {
			os.Exit(1)
		}
		err := writeFull(attestation, []byte(bootstrapAttestation))
		if closeErr := attestation.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Exit(1)
		}
		return
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
	request, err := hostprotocol.DecodeRequestFrom(in)
	if err != nil {
		return writeResponse(out, hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}}, err)
	}
	result, operationErr := runtime.Bootstrap(context.Background(), request)
	if operationErr != nil {
		return writeResponse(out, responseForOperation(operationErr), nil)
	}
	frame, err := hostprotocol.EncodeResponse(hostprotocol.Response{Result: &result})
	if err != nil {
		return err
	}
	return writeFull(out, frame)
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
	} else {
		response = responseForOperation(operationErr)
	}
	return writeResponse(out, response, nil)
}

func responseForOperation(err error) hostprotocol.Response {
	response := hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorRemoteOperation, Code: hostprotocol.CodeOperationFailed}}
	if remote, ok := err.(*hostruntime.RemoteError); ok {
		response = hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: remote.Category, Code: remote.Code}}
	}
	return response
}

// writeResponse is the sole stdout path so every accepted invocation has one frame.
func writeResponse(out io.Writer, response hostprotocol.Response, requestErr error) error {
	frame, err := hostprotocol.EncodeResponse(response)
	if err != nil {
		return err
	}
	if err = writeFull(out, frame); err != nil {
		return err
	}
	return requestErr
}

func writeFull(out io.Writer, b []byte) error {
	for len(b) != 0 {
		n, err := out.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
