package server

import (
	"bytes"
	"io"

	"github.com/gofiber/fiber/v2"
)

func init() {
	testMultipartBodyFallback = func(c *fiber.Ctx) io.Reader {
		return bytes.NewReader(c.Body())
	}
}
