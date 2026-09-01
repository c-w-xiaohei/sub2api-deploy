package hostruntime

import (
	"context"
	"io"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostprotocol"
)

func (r *Runtime) Serve(out io.Writer, in io.Reader) error {
	return r.serveRequest(out, in, r.Handle)
}

func (r *Runtime) ServeBootstrap(out io.Writer, in io.Reader) error {
	request, err := hostprotocol.DecodeRequestFrom(in)
	if err != nil {
		return writeResponse(out, hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: hostprotocol.ErrorProtocol, Code: hostprotocol.CodeMalformedFrame}}, err)
	}
	result, operationErr := r.Bootstrap(context.Background(), request)
	if operationErr != nil {
		return writeResponse(out, responseForOperation(operationErr), nil)
	}
	frame, err := hostprotocol.EncodeResponse(hostprotocol.Response{Result: &result})
	if err != nil {
		return err
	}
	return writeFull(out, frame)
}

func (r *Runtime) serveRequest(out io.Writer, in io.Reader, handle func(context.Context, hostprotocol.Request) (hostprotocol.Result, error)) error {
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
	if remote, ok := err.(*RemoteError); ok {
		response = hostprotocol.Response{Error: &hostprotocol.RemoteError{Category: remote.Category, Code: remote.Code}}
	}
	return response
}

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
