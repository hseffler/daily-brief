package main

import (
	"io"

	"github.com/sirupsen/logrus"
)

// testLogger returns a logger that discards output, for tests.
func testLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	logger.SetLevel(logrus.DebugLevel)
	return logger
}
