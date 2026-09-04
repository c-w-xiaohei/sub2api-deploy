package main

import (
	"io"
	"os"

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
		attestationBytes := []byte(bootstrapAttestation)
		var err error
		for len(attestationBytes) != 0 {
			n, writeErr := attestation.Write(attestationBytes)
			if writeErr != nil {
				err = writeErr
				break
			}
			if n <= 0 || n > len(attestationBytes) {
				err = io.ErrShortWrite
				break
			}
			attestationBytes = attestationBytes[n:]
		}
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
	return runtime.Serve(out, in)
}

func bootstrapServe(out io.Writer, in io.Reader, runtime *hostruntime.Runtime) error {
	return runtime.ServeBootstrap(out, in)
}
