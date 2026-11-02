package server

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/valyala/fasthttp"
)

func TestDownloadResponseCleanupLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "download.bin")
	if err := os.WriteFile(path, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, byteRange string
		status                  int
		size                    int64
		streamWriter, sendError bool
	}{
		{name: "normal", method: "GET", status: 200, size: 10},
		{name: "range", method: "GET", byteRange: "bytes=2-5", status: 206, size: 4},
		{name: "head", method: "HEAD", status: 200},
		{name: "unsatisfiable", method: "GET", byteRange: "bytes=20-30", status: 416},
		{name: "handler-error", method: "POST", status: 403},
		{name: "client-write-error", method: "GET", status: 200, size: 10, sendError: true},
		{name: "stream-writer-unknown-size", method: "GET", status: 200, size: -1, streamWriter: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := newRuntime(nil)
			defer runtime.cancel()
			server := &Server{transfers: newTransferRegistry()}
			app := fiber.New(fiber.Config{DisableStartupMessage: true})
			app.Use(runtime.requestAdmission)
			var original io.Reader
			app.All("/download", func(c *fiber.Ctx) error {
				if c.Method() == "POST" {
					return fiber.ErrForbidden
				}
				if test.streamWriter {
					c.Context().SetBodyStreamWriter(func(w *bufio.Writer) { _, _ = w.WriteString("stream") })
				} else if err := c.Download(path); err != nil {
					return err
				}
				original = c.Response().BodyStream()
				server.registerDownloadTransfer(c, "session", "default", "download.bin")
				return nil
			})
			var request fasthttp.Request
			request.SetRequestURI("/download")
			request.Header.SetMethod(test.method)
			if test.byteRange != "" {
				request.Header.Set("Range", test.byteRange)
			}
			var ctx fasthttp.RequestCtx
			ctx.Init(&request, nil, nil)
			defer ctx.Response.Reset()
			defer ctx.ResetUserValues()
			app.Handler()(&ctx)
			if ctx.Response.StatusCode() != test.status || runtime.active != 1 {
				t.Fatalf("after handler: status=%d active=%d", ctx.Response.StatusCode(), runtime.active)
			}
			if ctx.Response.BodyStream() != original {
				t.Fatal("registration replaced the response stream")
			}
			records := server.transfers.list()
			if test.size == 0 {
				if len(records) != 0 {
					t.Fatalf("bodyless/error response registered: %+v", records)
				}
			} else {
				if len(records) != 1 || records[0].TotalBytes != test.size || records[0].Status != transferActive || records[0].Cancelable || !records[0].BestEffort || records[0].TransferredBytes != 0 || records[0].CurrentSpeedBps != 0 {
					t.Fatalf("unexpected active response record: %+v", records)
				}
				if !test.streamWriter {
					if _, ok := original.(io.WriterTo); !ok {
						t.Fatal("file stream lost WriteTo")
					}
					if len(ctx.Response.Header.Peek("Content-Disposition")) == 0 {
						t.Fatal("download lost Content-Disposition")
					}
				}
			}
			var output bytes.Buffer
			var writer io.Writer = &output
			if test.sendError {
				reader, pipe := io.Pipe()
				_ = reader.Close()
				defer pipe.Close()
				writer = pipe
			}
			_, err := ctx.Response.WriteTo(writer)
			if (err != nil) != test.sendError {
				t.Fatalf("write response: %v", err)
			}
			// fasthttp runs this after response processing, also on write failures.
			ctx.ResetUserValues()
			if runtime.active != 0 || len(server.transfers.list()) != 0 {
				t.Fatalf("cleanup leaked admission/download: active=%d records=%+v", runtime.active, server.transfers.list())
			}
		})
	}
}
