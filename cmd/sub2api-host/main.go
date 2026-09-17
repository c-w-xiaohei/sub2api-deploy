package main

import (
	"io"
	"os"

	"github.com/c-w-xiaohei/sub2api-deploy/internal/hostruntime"
	"github.com/c-w-xiaohei/sub2api-deploy/internal/openssh"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "probe" && os.Args[1] != "stdio") {
		os.Exit(2)
	}
	if os.Args[1] == "probe" {
		record, err := openssh.LocalProbeRecord("")
		if err != nil {
			os.Exit(1)
		}
		if _, err := os.Stdout.Write(record); err != nil {
			os.Exit(1)
		}
		return
	}
	runtime := hostruntime.New("", "")
	err := serve(os.Stdout, os.Stdin, runtime)
	if err != nil {
		os.Exit(1)
	}
}

func serve(out io.Writer, in io.Reader, runtime *hostruntime.Runtime) error {
	return runtime.Serve(out, in)
}
